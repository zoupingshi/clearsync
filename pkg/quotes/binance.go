package quotes

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	gobinance "github.com/adshao/go-binance/v2"
	"github.com/ipfs/go-log/v2"
	"github.com/shopspring/decimal"

	"github.com/layer-3/clearsync/pkg/safe"
)

var (
	loggerBinance = log.Logger("binance")
	cexConfigured = atomic.Bool{}
)

type binance struct {
	once               *once
	usdcToUSDT         bool
	assetsUpdatePeriod time.Duration
	idlePeriod         time.Duration
	binanceClient      *gobinance.Client
	filter             Filter
	history            HistoricalData
	batcherInbox       chan<- TradeEvent
	outbox             chan<- TradeEvent
	streams            safe.Map[Market, chan struct{}]
	symbolToMarket     safe.Map[string, Market]
	assets             safe.Map[Market, gobinance.Symbol]
}

func newBinance(config BinanceConfig, outbox chan<- TradeEvent, history HistoricalData) (Driver, error) {
	batcherInbox := make(chan TradeEvent, 1024)
	go batch(config.BatchPeriod, batcherInbox, outbox)

	driver := &binance{
		once:               newOnce(),
		usdcToUSDT:         config.USDCtoUSDT,
		assetsUpdatePeriod: config.AssetsUpdatePeriod,
		idlePeriod:         config.IdlePeriod,
		binanceClient:      gobinance.NewClient("", ""),
		filter:             NewFilter(config.Filter),
		history:            history,
		batcherInbox:       batcherInbox,
		outbox:             outbox,
		streams:            safe.NewMap[Market, chan struct{}](),
		symbolToMarket:     safe.NewMap[string, Market](),
		assets:             safe.NewMap[Market, gobinance.Symbol](),
	}

	driver.updateAssets()
	go func() {
		ticker := time.NewTicker(driver.assetsUpdatePeriod)
		defer ticker.Stop()
		for {
			<-ticker.C
			driver.updateAssets()
		}
	}()

	return driver, nil
}

func (b *binance) ActiveDrivers() []DriverType {
	return []DriverType{DriverBinance}
}

func (b *binance) ExchangeType() ExchangeType {
	return ExchangeTypeCEX
}

func (b *binance) Start() error {
	started := b.once.Start(func() {
		cexConfigured.CompareAndSwap(false, true)
	})
	if !started {
		return ErrAlreadyStarted
	}
	return nil
}

func (b *binance) Stop() error {
	stopped := b.once.Stop(func() {
		b.streams.Range(func(market Market, _ chan struct{}) bool {
			err := b.Unsubscribe(market)
			return err == nil
		})

		b.streams = safe.NewMap[Market, chan struct{}]()
		cexConfigured.CompareAndSwap(true, false)
	})

	if !stopped {
		return ErrAlreadyStopped
	}
	return nil
}

func (b *binance) Subscribe(market Market) error {
	if !b.once.Subscribe() {
		return ErrNotStarted
	}

	if b.usdcToUSDT && market.Quote() == "usd" {
		if err := b.Subscribe(NewMarket(market.Base(), "usdt")); err != nil {
			loggerBinance.Warnw("failed to subscribe to USDT", "market", market, "error", err)
		}

		if err := b.Subscribe(NewMarket(market.Base(), "usdc")); err != nil {
			loggerBinance.Warnw("failed to subscribe to USDC", "market", market, "error", err)
		}
		return nil
	}

	symbol := strings.ToLower(market.Base() + market.Quote())
	b.symbolToMarket.Store(symbol, market)

	if _, ok := b.streams.Load(market); ok {
		return fmt.Errorf("%s: %w", market, ErrAlreadySubbed)
	}

	if _, ok := b.assets.Load(market); !ok {
		return fmt.Errorf("market does not exist: %s", market)
	}

	idle := time.NewTimer(b.idlePeriod)
	doneCh, stopCh, err := gobinance.WsAggTradeServe(symbol, b.handleTrade(market, idle), b.handleErr(market))
	if err != nil {
		return fmt.Errorf("%s: %w: %w", market, ErrFailedSub, err)
	}
	b.streams.Store(market, stopCh)

	go func() {
		defer idle.Stop()

		select {
		case <-doneCh:
			loggerBinance.Warnw("market stopped", "market", market)
		case <-idle.C:
			loggerBinance.Warnw("market inactivity detected", "market", market)
		}

		if _, ok := b.streams.Load(market); !ok {
			return // market was unsubscribed earlier
		}

		loggerBinance.Infow("resubscribing", "market", market)
		if err := b.Unsubscribe(market); err != nil {
			loggerBinance.Errorw("failed to unsubscribe", "market", market, "error", err)
		}

		for {
			err := b.Subscribe(market)
			if err == nil {
				loggerBinance.Infow("resubscribed", "market", market)
				break
			}
			loggerBinance.Errorw("failed to resubscribe", "market", market, "error", err)
			<-time.After(5 * time.Second)
		}
	}()

	recordSubscribed(DriverBinance, market)
	loggerBinance.Infow("subscribed", "market", market)
	return nil
}

func (b *binance) Unsubscribe(market Market) error {
	if !b.once.Unsubscribe() {
		return ErrNotStarted
	}

	stopCh, ok := b.streams.Load(market)
	if !ok {
		return fmt.Errorf("%s: %w", market, ErrNotSubbed)
	}

	stopCh <- struct{}{}
	close(stopCh)

	b.streams.Delete(market)
	recordUnsubscribed(DriverBinance, market)
	return nil
}

func (b *binance) updateAssets() {
	exchangeInfoService := b.binanceClient.NewExchangeInfoService()

	var exchangeInfo *gobinance.ExchangeInfo
	var err error
	for {
		exchangeInfo, err = exchangeInfoService.Do(context.Background())
		if err == nil {
			break
		}
		loggerBinance.Errorw("failed to fetch exchange info", "error", err)
		<-time.After(5 * time.Second)
		continue
	}

	for _, symbol := range exchangeInfo.Symbols {
		if symbol.Status != "TRADING" { // only interested in active pairs
			continue
		}

		market := NewMarket(symbol.BaseAsset, symbol.QuoteAsset)
		b.assets.Store(market, symbol)
	}
}

func (b *binance) handleTrade(
	market Market,
	idle *time.Timer,
) func(*gobinance.WsAggTradeEvent) {
	return func(event *gobinance.WsAggTradeEvent) {
		idle.Reset(b.idlePeriod)

		tradeEvent, err := b.buildEvent(event, market)
		if err != nil {
			loggerBinance.Errorw("failed to build trade event", "event", event, "error", err)
			return
		}

		if !b.filter.Allow(tradeEvent) {
			return
		}
		b.batcherInbox <- tradeEvent
	}
}

func (b *binance) handleErr(market Market) func(error) {
	return func(err error) {
		loggerBinance.Errorw("received error", "market", market, "error", err)
		if err.Error() == "websocket: close 1001 (going away)" || err.Error() == "websocket: close 1006 (abnormal closure): unexpected EOF" {
			// Reconnect logic
			const maxRetries = 5
			b.Unsubscribe(market)
			for i := 0; i < maxRetries; i++ {
				loggerBinance.Infow("attempting to reconnect to market", "market", market, "attempt", i+1)
				if err = b.Subscribe(market); err == nil {
					loggerBinance.Infow("resubscribed successfully", "market", market)
					break
				}
				loggerBinance.Errorw("failed to resubscribe", "market", market, "error", err)
				time.Sleep(time.Second * time.Duration(1<<i)) // Exponential backoff
			}
			if err != nil {
				loggerBinance.Errorw("failed to reconnect after max retries", "market", market, "error", err)
			}
		}
	}
}

func (b *binance) buildEvent(tr *gobinance.WsAggTradeEvent, market Market) (TradeEvent, error) {
	price, err := decimal.NewFromString(tr.Price)
	if err != nil {
		return TradeEvent{}, fmt.Errorf("failed to parse price: %+v", tr.Price)
	}

	amount, err := decimal.NewFromString(tr.Quantity)
	if err != nil {
		return TradeEvent{}, fmt.Errorf("failed to parse quantity: %+v", tr.Quantity)
	}

	if b.usdcToUSDT && (market.quoteUnit == "usdt" || market.quoteUnit == "usdc") {
		market.quoteUnit = "usd"
	}

	// IsBuyerMaker: true => the trade was initiated by the sell-side; the buy-side was the order book already.
	// IsBuyerMaker: false => the trade was initiated by the buy-side; the sell-side was the order book already.
	takerType := TakerTypeBuy
	if tr.IsBuyerMaker {
		takerType = TakerTypeSell
	}

	return TradeEvent{
		Source:    DriverBinance,
		Market:    market,
		Price:     price,
		Amount:    amount,
		Total:     price.Mul(amount),
		TakerType: takerType,
		CreatedAt: time.UnixMilli(tr.TradeTime),
	}, nil
}

func (b *binance) HistoricalData(ctx context.Context, market Market, window time.Duration, limit uint64) ([]TradeEvent, error) {
	trades, err := fetchHistoryDataFromExternalSource(ctx, b.history, market, window, limit, loggerBinance)
	if err == nil && len(trades) > 0 {
		return trades, nil
	}

	// Fetch data
	aggTradesService := b.binanceClient.NewAggTradesService()

	aggTradesService.StartTime(time.Now().Add(-window).Unix() * 1000)
	aggTradesService.EndTime(time.Now().Unix() * 1000)
	aggTradesService.Limit(int(limit))

	base := strings.ToLower(market.Base())
	quote := strings.ToLower(market.Quote())

	tokenFixtures := map[string]string{
		"usd":  "usdt", // as of spring 2024 Binance does not provide USD spot markets
		"weth": "eth",  // as of spring 2024 Binance does not provide WETH spot markets
	}
	if newBase, ok := tokenFixtures[base]; ok {
		base = newBase
	}
	if newQuote, ok := tokenFixtures[quote]; ok {
		quote = newQuote
	}

	symbol := strings.ToUpper(base + quote)
	aggTradesService.Symbol(symbol)

	aggTrades, err := aggTradesService.Do(ctx)
	if err != nil {
		loggerBinance.Errorw("failed to fetch historical data", "market", market, "error", err)
		return nil, fmt.Errorf("failed to fetch historical data: %w", err)
	}

	// Convert aggregated trades to a trade events
	trades = make([]TradeEvent, 0, limit)
	for _, aggTrade := range aggTrades {
		trade, err := b.buildEvent(&gobinance.WsAggTradeEvent{
			Price:        aggTrade.Price,
			Quantity:     aggTrade.Quantity,
			TradeTime:    aggTrade.Timestamp,
			IsBuyerMaker: aggTrade.IsBuyerMaker,
		}, market)
		if err != nil {
			loggerBinance.Errorw("failed to build trade event", "market", market, "error", err)
			continue
		}
		if trade.Price.IsZero() {
			loggerBinance.Warnw("skipping trade with zero price",
				"market", market,
				"trade", trade,
				"aggregated_trade", aggTrade)
			continue
		}
		trades = append(trades, trade)
	}

	sortTradeEventsInPlace(trades)
	return trades, nil
}

func batch(batchPeriod time.Duration, inbox <-chan TradeEvent, outbox chan<- TradeEvent) {
	marketTrades := make(map[Market][]TradeEvent)
	timer := time.NewTimer(batchPeriod)
	defer timer.Stop()

	for {
		select {
		case trade := <-inbox:
			marketTrades[trade.Market] = append(marketTrades[trade.Market], trade)
		case <-timer.C:
			for market, trades := range marketTrades {
				if event := combineTrades(trades); event != nil {
					marketTrades[market] = nil
					outbox <- *event
				}
			}
			timer.Reset(batchPeriod)
		}
	}
}

func combineTrades(trades []TradeEvent) *TradeEvent {
	if len(trades) == 0 {
		return nil
	}

	totalAmount := decimal.Zero
	totalValue := decimal.Zero
	netAmount := decimal.Zero

	for _, trade := range trades {
		totalAmount = totalAmount.Add(trade.Amount)
		totalValue = totalValue.Add(trade.Amount.Mul(trade.Price))

		// Update net amount to determine net side (buy or sell)
		if trade.TakerType == TakerTypeBuy {
			netAmount = netAmount.Add(trade.Amount)
		} else if trade.TakerType == TakerTypeSell {
			netAmount = netAmount.Sub(trade.Amount)
		}
	}

	if totalAmount.IsZero() {
		return nil
	}

	avgPrice := totalValue.Div(totalAmount)
	// Determine net side (buy or sell)
	var side TakerType
	if netAmount.GreaterThanOrEqual(decimal.Zero) {
		side = TakerTypeSell // "buy" (yes, it looks inverted)
	} else {
		side = TakerTypeBuy // "sell"
	}

	return &TradeEvent{
		Source:    trades[0].Source,
		Market:    trades[0].Market,
		Price:     avgPrice,
		Amount:    totalAmount,
		Total:     avgPrice.Mul(totalAmount),
		TakerType: side,
		CreatedAt: time.Now(),
	}
}
