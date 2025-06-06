package mm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/mm/libxc"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/calc"
	"decred.org/dcrdex/dex/encode"
	"decred.org/dcrdex/dex/order"
)

func TestArbRebalance(t *testing.T) {
	lotSize := uint64(40e8)
	baseID := uint32(42)
	quoteID := uint32(0)

	orderIDs := make([]order.OrderID, 5)
	for i := 0; i < 5; i++ {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	cexTradeIDs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		cexTradeIDs = append(cexTradeIDs, fmt.Sprintf("%x", encode.RandomBytes(32)))
	}

	const currEpoch uint64 = 100
	const numEpochsLeaveOpen uint32 = 10
	const maxActiveArbs uint32 = 5
	const profitTrigger float64 = 0.01
	const feesInQuoteUnits uint64 = 5e5
	const rateStep = 1e5

	edgeSellRate := func(buyRate, qty uint64, profitable bool) uint64 {
		quoteToBuy := calc.BaseToQuote(buyRate, qty)
		reqFromSell := quoteToBuy + feesInQuoteUnits + calc.BaseToQuote(buyRate, uint64(float64(qty)*profitTrigger))
		sellRate := calc.QuoteToBase(qty, reqFromSell) // quote * 1e8 / base = sellRate
		var steps float64
		if profitable {
			steps = math.Ceil(float64(sellRate) / float64(rateStep))
		} else {
			steps = math.Floor(float64(sellRate) / float64(rateStep))
		}
		return uint64(steps) * rateStep
	}

	type testBooks struct {
		dexBidsAvg     []uint64
		dexBidsExtrema []uint64

		dexAsksAvg     []uint64
		dexAsksExtrema []uint64

		cexBidsAvg     []uint64
		cexBidsExtrema []uint64

		cexAsksAvg     []uint64
		cexAsksExtrema []uint64
	}

	noArbBooks := &testBooks{
		dexBidsAvg:     []uint64{1.8e6, 1.7e6},
		dexBidsExtrema: []uint64{1.7e6, 1.6e6},

		dexAsksAvg:     []uint64{2e6, 2.5e6},
		dexAsksExtrema: []uint64{2e6, 3e6},

		cexBidsAvg:     []uint64{edgeSellRate(2e6, lotSize, false), 2.1e6},
		cexBidsExtrema: []uint64{2.2e6, 1.9e6},

		cexAsksAvg:     []uint64{2.4e6, 2.6e6},
		cexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	arbBuyOnDEXBooks := &testBooks{
		dexBidsAvg:     []uint64{1.8e6, 1.7e6},
		dexBidsExtrema: []uint64{1.7e6, 1.6e6},

		dexAsksAvg:     []uint64{2e6, 2.5e6},
		dexAsksExtrema: []uint64{2e6, 3e6},

		cexBidsAvg:     []uint64{edgeSellRate(2e6, lotSize, true), 2.1e6},
		cexBidsExtrema: []uint64{2.2e6, 1.9e6},

		cexAsksAvg:     []uint64{2.4e6, 2.6e6},
		cexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	arbSellOnDEXBooks := &testBooks{
		cexBidsAvg:     []uint64{1.8e6, 1.7e6},
		cexBidsExtrema: []uint64{1.7e6, 1.6e6},

		cexAsksAvg:     []uint64{2e6, 2.5e6},
		cexAsksExtrema: []uint64{2e6, 3e6},

		dexBidsAvg:     []uint64{edgeSellRate(2e6, lotSize, true), 2.1e6},
		dexBidsExtrema: []uint64{2.2e6, 1.9e6},

		dexAsksAvg:     []uint64{2.4e6, 2.6e6},
		dexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	arb2LotsBuyOnDEXBooks := &testBooks{
		dexBidsAvg:     []uint64{1.8e6, 1.7e6},
		dexBidsExtrema: []uint64{1.7e6, 1.6e6},

		dexAsksAvg:     []uint64{2e6, 2e6, 2.5e6},
		dexAsksExtrema: []uint64{2e6, 2e6, 3e6},

		cexBidsAvg:     []uint64{2.3e6, 2.2e6, 2.1e6},
		cexBidsExtrema: []uint64{2.2e6, 2.2e6, 1.9e6},

		cexAsksAvg:     []uint64{2.4e6, 2.6e6},
		cexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	arb2LotsSellOnDEXBooks := &testBooks{
		cexBidsAvg:     []uint64{1.8e6, 1.7e6},
		cexBidsExtrema: []uint64{1.7e6, 1.6e6},

		cexAsksAvg:     []uint64{2e6, 2e6, 2.5e6},
		cexAsksExtrema: []uint64{2e6, 2e6, 3e6},

		dexBidsAvg:     []uint64{edgeSellRate(2e6, lotSize, true), edgeSellRate(2e6, lotSize, true), 2.1e6},
		dexBidsExtrema: []uint64{2.2e6, 2.2e6, 1.9e6},

		dexAsksAvg:     []uint64{2.4e6, 2.6e6},
		dexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	// Arbing 2 lots worth would still be above profit trigger, but the
	// second lot on its own would not be.
	arb2LotsButOneWorth := &testBooks{
		dexBidsAvg:     []uint64{1.8e6, 1.7e6},
		dexBidsExtrema: []uint64{1.7e6, 1.6e6},

		dexAsksAvg:     []uint64{2e6, 2.1e6},
		dexAsksExtrema: []uint64{2e6, 2.2e6},

		cexBidsAvg:     []uint64{2.3e6, 2.122e6},
		cexBidsExtrema: []uint64{2.2e6, 2.1e6},

		cexAsksAvg:     []uint64{2.4e6, 2.6e6},
		cexAsksExtrema: []uint64{2.5e6, 2.7e6},
	}

	type test struct {
		name          string
		books         *testBooks
		dexVWAPErr    error
		cexVWAPErr    error
		cexTradeErr   error
		existingArbs  []*arbSequence
		dexMaxBuyQty  uint64
		dexMaxSellQty uint64
		cexMaxBuyQty  uint64
		cexMaxSellQty uint64

		expectedDexOrder   *dexOrder
		expectedCexOrder   *libxc.Trade
		expectedDEXCancels []dex.Bytes
		expectedCEXCancels []string
	}

	tests := []test{
		// "no arb"
		{
			name:  "no arb",
			books: noArbBooks,
		},
		// "1 lot, buy on dex, sell on cex"
		{
			name:          "1 lot, buy on dex, sell on cex",
			books:         arbBuyOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			expectedDexOrder: &dexOrder{
				qty:  lotSize,
				rate: 2e6,
				sell: false,
			},
			expectedCexOrder: &libxc.Trade{
				BaseID:  42,
				QuoteID: 0,
				Qty:     lotSize,
				Rate:    2.2e6,
				Sell:    true,
			},
		},
		// "1 lot, sell on dex, buy on cex"
		{
			name:          "1 lot, sell on dex, buy on cex",
			books:         arbSellOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			expectedDexOrder: &dexOrder{
				qty:  lotSize,
				rate: 2.2e6,
				sell: true,
			},
			expectedCexOrder: &libxc.Trade{
				BaseID:  42,
				QuoteID: 0,
				Qty:     lotSize,
				Rate:    2e6,
				Sell:    false,
			},
		},
		// "1 lot, buy on dex, sell on cex, but dex base balance not enough"
		{
			name:          "1 lot, buy on dex, sell on cex, but cex balance not enough",
			books:         arbBuyOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 0,
			cexMaxBuyQty:  5 * lotSize,
		},
		// "2 lot, buy on dex, sell on cex, but dex quote balance only enough for 1"
		{
			name:          "2 lot, buy on dex, sell on cex, but dex quote balance only enough for 1",
			books:         arb2LotsBuyOnDEXBooks,
			dexMaxBuyQty:  1 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			expectedDexOrder: &dexOrder{
				qty:  lotSize,
				rate: 2e6,
				sell: false,
			},
			expectedCexOrder: &libxc.Trade{
				BaseID:  42,
				QuoteID: 0,
				Qty:     lotSize,
				Rate:    2.2e6,
				Sell:    true,
			},
		},
		// "2 lot, buy on cex, sell on dex, but cex quote balance only enough for 1"
		{
			name:          "2 lot, buy on cex, sell on dex, but cex quote balance only enough for 1",
			books:         arb2LotsSellOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  lotSize,
			expectedDexOrder: &dexOrder{
				qty:  lotSize,
				rate: 2.2e6,
				sell: true,
			},
			expectedCexOrder: &libxc.Trade{
				BaseID:  42,
				QuoteID: 0,
				Qty:     lotSize,
				Rate:    2e6,
				Sell:    false,
			},
		},
		// "2 lots arb still above profit trigger, but second not worth it on its own"
		{
			name:          "2 lots arb still above profit trigger, but second not worth it on its own",
			books:         arb2LotsButOneWorth,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			expectedDexOrder: &dexOrder{
				qty:  lotSize,
				rate: 2e6,
				sell: false,
			},
			expectedCexOrder: &libxc.Trade{
				BaseID:  42,
				QuoteID: 0,
				Qty:     lotSize,
				Rate:    2.2e6,
				Sell:    true,
			},
		},
		// "cex no asks"
		{
			name: "cex no asks",
			books: &testBooks{
				dexBidsAvg:     []uint64{1.8e6, 1.7e6},
				dexBidsExtrema: []uint64{1.7e6, 1.6e6},

				dexAsksAvg:     []uint64{2e6, 2.5e6},
				dexAsksExtrema: []uint64{2e6, 3e6},

				cexBidsAvg:     []uint64{1.9e6, 1.8e6},
				cexBidsExtrema: []uint64{1.85e6, 1.75e6},

				cexAsksAvg:     []uint64{},
				cexAsksExtrema: []uint64{},
			},
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
		},
		// "dex no asks"
		{
			name: "dex no asks",
			books: &testBooks{
				dexBidsAvg:     []uint64{1.8e6, 1.7e6},
				dexBidsExtrema: []uint64{1.7e6, 1.6e6},

				dexAsksAvg:     []uint64{},
				dexAsksExtrema: []uint64{},

				cexBidsAvg:     []uint64{1.9e6, 1.8e6},
				cexBidsExtrema: []uint64{1.85e6, 1.75e6},

				cexAsksAvg:     []uint64{2.1e6, 2.2e6},
				cexAsksExtrema: []uint64{2.2e6, 2.3e6},
			},
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
		},
		// "self-match"
		{
			name:  "self-match",
			books: arbSellOnDEXBooks,
			existingArbs: []*arbSequence{{
				dexOrder: &core.Order{
					ID:   orderIDs[0][:],
					Rate: 2.2e6,
				},
				cexOrderID: cexTradeIDs[0],
				sellOnDEX:  false,
				startEpoch: currEpoch - 2,
			}},
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,

			expectedCEXCancels: []string{cexTradeIDs[0]},
			expectedDEXCancels: []dex.Bytes{orderIDs[0][:]},
		},
		// "remove expired active arbs"
		{
			name:          "remove expired active arbs",
			books:         noArbBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			existingArbs: []*arbSequence{
				{
					dexOrder: &core.Order{
						ID: orderIDs[0][:],
					},
					cexOrderID: cexTradeIDs[0],
					sellOnDEX:  false,
					startEpoch: currEpoch - 2,
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[1][:],
					},
					cexOrderID: cexTradeIDs[1],
					sellOnDEX:  false,
					startEpoch: currEpoch - (uint64(numEpochsLeaveOpen) + 2),
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[2][:],
					},
					cexOrderID:     cexTradeIDs[2],
					sellOnDEX:      false,
					cexOrderFilled: true,
					startEpoch:     currEpoch - (uint64(numEpochsLeaveOpen) + 2),
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[3][:],
					},
					cexOrderID:     cexTradeIDs[3],
					sellOnDEX:      false,
					dexOrderFilled: true,
					startEpoch:     currEpoch - (uint64(numEpochsLeaveOpen) + 2),
				},
			},
			expectedCEXCancels: []string{cexTradeIDs[1], cexTradeIDs[3]},
			expectedDEXCancels: []dex.Bytes{orderIDs[1][:], orderIDs[2][:]},
		},
		// "already max active arbs"
		{
			name:          "already max active arbs",
			books:         arbBuyOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			existingArbs: []*arbSequence{
				{
					dexOrder: &core.Order{
						ID: orderIDs[0][:],
					},
					cexOrderID: cexTradeIDs[0],
					sellOnDEX:  false,
					startEpoch: currEpoch - 1,
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[1][:],
					},
					cexOrderID: cexTradeIDs[2],
					sellOnDEX:  false,
					startEpoch: currEpoch - 2,
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[2][:],
					},
					cexOrderID: cexTradeIDs[2],
					sellOnDEX:  false,
					startEpoch: currEpoch - 3,
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[3][:],
					},
					cexOrderID: cexTradeIDs[3],
					sellOnDEX:  false,
					startEpoch: currEpoch - 4,
				},
				{
					dexOrder: &core.Order{
						ID: orderIDs[4][:],
					},
					cexOrderID: cexTradeIDs[4],
					sellOnDEX:  false,
					startEpoch: currEpoch - 5,
				},
			},
		},
		// "cex trade error"
		{
			name:          "cex trade error",
			books:         arbBuyOnDEXBooks,
			dexMaxSellQty: 5 * lotSize,
			dexMaxBuyQty:  5 * lotSize,
			cexMaxSellQty: 5 * lotSize,
			cexMaxBuyQty:  5 * lotSize,
			cexTradeErr:   errors.New(""),
		},
	}

	runTest := func(test *test) {
		t.Run(test.name, func(t *testing.T) {
			cex := newTBotCEXAdaptor()
			tcex := newTCEX()
			tcex.vwapErr = test.cexVWAPErr
			cex.tradeErr = test.cexTradeErr
			cex.maxBuyQty = test.cexMaxBuyQty
			cex.maxSellQty = test.cexMaxSellQty

			tc := newTCore()
			coreAdaptor := newTBotCoreAdaptor(tc)
			coreAdaptor.buyFeesInQuote = feesInQuoteUnits
			coreAdaptor.sellFeesInQuote = feesInQuoteUnits
			coreAdaptor.maxBuyQty = test.dexMaxBuyQty
			coreAdaptor.maxSellQty = test.dexMaxSellQty

			if test.expectedDexOrder != nil {
				coreAdaptor.tradeResult = &core.Order{
					Qty:  test.expectedDexOrder.qty,
					Rate: test.expectedDexOrder.rate,
					Sell: test.expectedDexOrder.sell,
				}
			}

			orderBook := &tOrderBook{
				bidsVWAP: make(map[uint64]vwapResult),
				asksVWAP: make(map[uint64]vwapResult),
				vwapErr:  test.dexVWAPErr,
			}
			for i := range test.books.dexBidsAvg {
				orderBook.bidsVWAP[uint64(i+1)] = vwapResult{test.books.dexBidsAvg[i], test.books.dexBidsExtrema[i]}
			}
			for i := range test.books.dexAsksAvg {
				orderBook.asksVWAP[uint64(i+1)] = vwapResult{test.books.dexAsksAvg[i], test.books.dexAsksExtrema[i]}
			}
			for i := range test.books.cexBidsAvg {
				tcex.bidsVWAP[uint64(i+1)*lotSize] = vwapResult{test.books.cexBidsAvg[i], test.books.cexBidsExtrema[i]}
			}
			for i := range test.books.cexAsksAvg {
				tcex.asksVWAP[uint64(i+1)*lotSize] = vwapResult{test.books.cexAsksAvg[i], test.books.cexAsksExtrema[i]}
			}

			u := mustParseAdaptorFromMarket(&core.Market{
				LotSize:  lotSize,
				BaseID:   baseID,
				QuoteID:  quoteID,
				RateStep: 1e2,
			})
			u.clientCore.(*tCore).userParcels = 0
			u.clientCore.(*tCore).parcelLimit = 1

			a := &simpleArbMarketMaker{
				unifiedExchangeAdaptor: u,
				cex:                    cex,
				core:                   coreAdaptor,
				activeArbs:             test.existingArbs,
			}
			const sellSwapFees, sellRedeemFees = 3e5, 1e5
			const buySwapFees, buyRedeemFees = 2e4, 1e4
			const buyRate, sellRate = 1e7, 1.1e7
			a.CEX = tcex
			a.buyFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: buyRedeemFees,
					},
					Estimated: &LotFees{
						Swap:   buySwapFees,
						Redeem: buyRedeemFees,
					},
				},
				BookingFeesPerLot: buySwapFees,
			}
			a.sellFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: sellRedeemFees,
					},
					Estimated: &LotFees{
						Swap:   sellSwapFees,
						Redeem: sellRedeemFees,
					},
				},
				BookingFeesPerLot: sellSwapFees,
			}
			// arbEngine.setBotLoop(arbEngine.botLoop)
			a.unifiedExchangeAdaptor.botCfgV.Store(&BotConfig{
				SimpleArbConfig: &SimpleArbConfig{
					ProfitTrigger:      profitTrigger,
					MaxActiveArbs:      maxActiveArbs,
					NumEpochsLeaveOpen: numEpochsLeaveOpen,
				},
			})
			a.book = orderBook
			a.rebalance(currEpoch)

			// Check dex trade
			if test.expectedDexOrder == nil != (coreAdaptor.lastTradePlaced == nil) {
				t.Fatalf("%s: expected dex order %v but got %v", test.name, (test.expectedDexOrder != nil), (coreAdaptor.lastTradePlaced != nil))
			}
			if test.expectedDexOrder != nil {
				if test.expectedDexOrder.rate != coreAdaptor.lastTradePlaced.rate {
					t.Fatalf("%s: expected sell order rate %d but got %d", test.name, test.expectedDexOrder.rate, coreAdaptor.lastTradePlaced.rate)
				}
				if test.expectedDexOrder.qty != coreAdaptor.lastTradePlaced.qty {
					t.Fatalf("%s: expected sell order qty %d but got %d", test.name, test.expectedDexOrder.qty, coreAdaptor.lastTradePlaced.qty)
				}
				if test.expectedDexOrder.sell != coreAdaptor.lastTradePlaced.sell {
					t.Fatalf("%s: expected sell order sell %v but got %v", test.name, test.expectedDexOrder.sell, coreAdaptor.lastTradePlaced.sell)
				}
			}

			// Check cex trade
			if (test.expectedCexOrder == nil) != (cex.lastTrade == nil) {
				t.Fatalf("%s: expected cex order %v but got %v", test.name, (test.expectedCexOrder != nil), (cex.lastTrade != nil))
			}
			if cex.lastTrade != nil &&
				*cex.lastTrade != *test.expectedCexOrder {
				t.Fatalf("%s: cex order %+v != expected %+v", test.name, cex.lastTrade, test.expectedCexOrder)
			}

			// Check dex cancels
			if len(test.expectedDEXCancels) != len(tc.cancelsPlaced) {
				t.Fatalf("%s: expected %d cancels but got %d", test.name, len(test.expectedDEXCancels), len(tc.cancelsPlaced))
			}
			for i := range test.expectedDEXCancels {
				if !bytes.Equal(test.expectedDEXCancels[i], tc.cancelsPlaced[i][:]) {
					t.Fatalf("%s: expected cancel %x but got %x", test.name, test.expectedDEXCancels[i], tc.cancelsPlaced[i])
				}
			}

			// Check cex cancels
			if len(test.expectedCEXCancels) != len(cex.cancelledTrades) {
				t.Fatalf("%s: expected %d cex cancels but got %d", test.name, len(test.expectedCEXCancels), len(cex.cancelledTrades))
			}
			for i := range test.expectedCEXCancels {
				if test.expectedCEXCancels[i] != cex.cancelledTrades[i] {
					t.Fatalf("%s: expected cex cancel %s but got %s", test.name, test.expectedCEXCancels[i], cex.cancelledTrades[i])
				}
			}
		})
	}

	for _, test := range tests {
		runTest(&test)
	}
}

func TestArbDexTradeUpdates(t *testing.T) {
	orderIDs := make([]order.OrderID, 5)
	for i := 0; i < 5; i++ {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	cexTradeIDs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		cexTradeIDs = append(cexTradeIDs, fmt.Sprintf("%x", encode.RandomBytes(32)))
	}

	type test struct {
		name               string
		activeArbs         []*arbSequence
		updatedOrderID     []byte
		updatedOrderStatus order.OrderStatus
		expectedActiveArbs []*arbSequence
	}

	dexOrder := &core.Order{
		ID: orderIDs[0][:],
	}

	tests := []*test{
		{
			name: "dex order still booked",
			activeArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
			updatedOrderID:     orderIDs[0][:],
			updatedOrderStatus: order.OrderStatusBooked,
			expectedActiveArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
		},
		{
			name: "dex order executed, but cex not yet filled",
			activeArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
			updatedOrderID:     orderIDs[0][:],
			updatedOrderStatus: order.OrderStatusExecuted,
			expectedActiveArbs: []*arbSequence{
				{
					dexOrder:       dexOrder,
					cexOrderID:     cexTradeIDs[0],
					dexOrderFilled: true,
				},
			},
		},
		{
			name: "dex order executed, but cex already filled",
			activeArbs: []*arbSequence{
				{
					dexOrder:       dexOrder,
					cexOrderID:     cexTradeIDs[0],
					cexOrderFilled: true,
				},
			},
			updatedOrderID:     orderIDs[0][:],
			updatedOrderStatus: order.OrderStatusExecuted,
			expectedActiveArbs: []*arbSequence{},
		},
	}

	runTest := func(test *test) {
		cex := newTBotCEXAdaptor()
		coreAdaptor := newTBotCoreAdaptor(newTCore())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		arbEngine := &simpleArbMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				BaseID:  42,
				QuoteID: 0,
			}),
			cex:        cex,
			core:       coreAdaptor,
			activeArbs: test.activeArbs,
		}
		arbEngine.clientCore = newTCore()
		arbEngine.CEX = newTCEX()
		arbEngine.ctx = ctx
		arbEngine.setBotLoop(arbEngine.botLoop)
		arbEngine.unifiedExchangeAdaptor.botCfgV.Store(&BotConfig{
			SimpleArbConfig: &SimpleArbConfig{
				ProfitTrigger:      0.01,
				MaxActiveArbs:      5,
				NumEpochsLeaveOpen: 10,
			},
		})

		err := arbEngine.runBotLoop(ctx)
		if err != nil {
			t.Fatalf("%s: Connect error: %v", test.name, err)
		}

		coreAdaptor.orderUpdates <- &core.Order{
			Status: test.updatedOrderStatus,
			ID:     test.updatedOrderID,
		}
		coreAdaptor.orderUpdates <- &core.Order{}

		if len(test.expectedActiveArbs) != len(arbEngine.activeArbs) {
			t.Fatalf("%s: expected %d active arbs but got %d", test.name, len(test.expectedActiveArbs), len(arbEngine.activeArbs))
		}

		for i := range test.expectedActiveArbs {
			if *arbEngine.activeArbs[i] != *test.expectedActiveArbs[i] {
				t.Fatalf("%s: active arb %+v != expected active arb %+v", test.name, arbEngine.activeArbs[i], test.expectedActiveArbs[i])
			}
		}
	}

	for _, test := range tests {
		runTest(test)
	}
}

func TestCexTradeUpdates(t *testing.T) {
	orderIDs := make([]order.OrderID, 5)
	for i := 0; i < 5; i++ {
		copy(orderIDs[i][:], encode.RandomBytes(32))
	}

	cexTradeIDs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		cexTradeIDs = append(cexTradeIDs, fmt.Sprintf("%x", encode.RandomBytes(32)))
	}

	dexOrder := &core.Order{
		ID: orderIDs[0][:],
	}

	type test struct {
		name               string
		activeArbs         []*arbSequence
		updatedOrderID     string
		orderComplete      bool
		expectedActiveArbs []*arbSequence
	}

	tests := []*test{
		{
			name: "neither complete",
			activeArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
			updatedOrderID: cexTradeIDs[0],
			orderComplete:  false,
			expectedActiveArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
		},
		{
			name: "cex complete, but dex order not complete",
			activeArbs: []*arbSequence{
				{
					dexOrder:   dexOrder,
					cexOrderID: cexTradeIDs[0],
				},
			},
			updatedOrderID: cexTradeIDs[0],
			orderComplete:  true,
			expectedActiveArbs: []*arbSequence{
				{
					dexOrder:       dexOrder,
					cexOrderID:     cexTradeIDs[0],
					cexOrderFilled: true,
				},
			},
		},
		{
			name: "both complete",
			activeArbs: []*arbSequence{
				{
					dexOrder:       dexOrder,
					cexOrderID:     cexTradeIDs[0],
					dexOrderFilled: true,
				},
			},
			updatedOrderID: cexTradeIDs[0],
			orderComplete:  true,
		},
	}

	runTest := func(test *test) {
		cex := newTBotCEXAdaptor()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		arbEngine := &simpleArbMarketMaker{
			unifiedExchangeAdaptor: mustParseAdaptorFromMarket(&core.Market{
				BaseID:  42,
				QuoteID: 0,
			}),
			cex:        cex,
			core:       newTBotCoreAdaptor(newTCore()),
			activeArbs: test.activeArbs,
		}
		arbEngine.ctx = ctx
		arbEngine.CEX = newTCEX()
		arbEngine.setBotLoop(arbEngine.botLoop)
		arbEngine.unifiedExchangeAdaptor.botCfgV.Store(&BotConfig{
			SimpleArbConfig: &SimpleArbConfig{
				ProfitTrigger:      0.01,
				MaxActiveArbs:      5,
				NumEpochsLeaveOpen: 10,
			},
		})

		err := arbEngine.runBotLoop(ctx)
		if err != nil {
			t.Fatalf("%s: Connect error: %v", test.name, err)
		}

		cex.tradeUpdates <- &libxc.Trade{
			ID:       test.updatedOrderID,
			Complete: test.orderComplete,
		}
		// send dummy update
		cex.tradeUpdates <- &libxc.Trade{
			ID: "",
		}

		if len(test.expectedActiveArbs) != len(arbEngine.activeArbs) {
			t.Fatalf("%s: expected %d active arbs but got %d", test.name, len(test.expectedActiveArbs), len(arbEngine.activeArbs))
		}
		for i := range test.expectedActiveArbs {
			if *arbEngine.activeArbs[i] != *test.expectedActiveArbs[i] {
				t.Fatalf("%s: active arb %+v != expected active arb %+v", test.name, arbEngine.activeArbs[i], test.expectedActiveArbs[i])
			}
		}
	}

	for _, test := range tests {
		runTest(test)
	}
}

/*func TestArbBotProblems(t *testing.T) {
	const baseID, quoteID = 42, 0
	const lotSize uint64 = 5e9
	const sellSwapFees, sellRedeemFees = 3e6, 1e6
	const buySwapFees, buyRedeemFees = 2e5, 1e5
	const buyRate, sellRate = 1e7, 1.1e7

	type test struct {
		name            string
		userLimitTooLow bool
		dexBalanceDefs  map[uint32]uint64
		cexBalanceDefs  map[uint32]uint64

		expBotProblems *BotProblems
	}

	updateBotProblems := func(f func(*BotProblems)) *BotProblems {
		bp := newBotProblems()
		f(bp)
		return bp
	}

	tests := []*test{
		{
			name:           "no problems",
			expBotProblems: newBotProblems(),
		},
		{
			name:            "user limit too low",
			userLimitTooLow: true,
			expBotProblems: updateBotProblems(func(bp *BotProblems) {
				bp.UserLimitTooLow = true
			}),
		},
		{
			name: "balance deficiencies",
			dexBalanceDefs: map[uint32]uint64{
				baseID:  lotSize + sellSwapFees,
				quoteID: calc.BaseToQuote(buyRate, lotSize) + buySwapFees,
			},
			cexBalanceDefs: map[uint32]uint64{
				baseID:  lotSize,
				quoteID: calc.BaseToQuote(sellRate, lotSize),
			},
			expBotProblems: updateBotProblems(func(bp *BotProblems) {
				// All these values are multiplied by 2 because the same deficiencies
				// are returned for buys and sells, and they are summed.
				bp.DEXBalanceDeficiencies = map[uint32]uint64{
					baseID:  (lotSize + sellSwapFees) * 2,
					quoteID: (calc.BaseToQuote(buyRate, lotSize) + buySwapFees) * 2,
				}
				bp.CEXBalanceDeficiencies = map[uint32]uint64{
					baseID:  lotSize * 2,
					quoteID: calc.BaseToQuote(sellRate, lotSize) * 2,
				}
			}),
		},
	}

	runTest := func(tt *test) {
		t.Run(tt.name, func(t *testing.T) {
			cex := newTCEX()
			mkt := &core.Market{
				RateStep:   1e3,
				AtomToConv: 1,
				LotSize:    lotSize,
				BaseID:     baseID,
				QuoteID:    quoteID,
			}
			u := mustParseAdaptorFromMarket(mkt)
			u.CEX = cex
			u.botCfgV.Store(&BotConfig{})
			c := newTCore()
			if !tt.userLimitTooLow {
				u.clientCore.(*tCore).userParcels = 0
				u.clientCore.(*tCore).parcelLimit = 1
			}
			u.fiatRates.Store(map[uint32]float64{baseID: 1, quoteID: 1})
			cexAdaptor := newTBotCEXAdaptor()
			coreAdaptor := newTBotCoreAdaptor(c)
			a := &simpleArbMarketMaker{
				unifiedExchangeAdaptor: u,
				cex:                    cexAdaptor,
				core:                   coreAdaptor,
			}

			coreAdaptor.balanceDefs = tt.dexBalanceDefs
			cexAdaptor.balanceDefs = tt.cexBalanceDefs

			a.cfgV.Store(&SimpleArbConfig{})

			cex.asksVWAP[lotSize] = vwapResult{
				avg:     buyRate,
				extrema: buyRate,
			}
			cex.bidsVWAP[lotSize] = vwapResult{
				avg:     sellRate,
				extrema: sellRate,
			}

			a.book = &tOrderBook{
				bidsVWAP: map[uint64]vwapResult{
					1: {
						avg:     buyRate,
						extrema: buyRate,
					},
				},
				asksVWAP: map[uint64]vwapResult{
					1: {
						avg:     sellRate,
						extrema: sellRate,
					},
				},
			}

			a.buyFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: buyRedeemFees,
						Swap:   buySwapFees,
					},
					Estimated: &LotFees{},
				},
				BookingFeesPerLot: buySwapFees,
			}
			a.sellFees = &OrderFees{
				LotFeeRange: &LotFeeRange{
					Max: &LotFees{
						Redeem: sellRedeemFees,
						Swap:   sellSwapFees,
					},
					Estimated: &LotFees{},
				},
				BookingFeesPerLot: sellSwapFees,
			}

			a.rebalance(1)

			problems := a.problems()
			if !reflect.DeepEqual(tt.expBotProblems, problems) {
				t.Fatalf("expected bot problems %v, got %v", tt.expBotProblems, problems)
			}
		})
	}

	for _, test := range tests {
		runTest(test)
	}
}
*/
