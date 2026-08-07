package risk

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// The two live episodes of 7 August, replayed from the prices in the log.
//
// The trade channel carries neither fee nor pnl, so both are computed here: the
// rate comes from is_taker, the money from the prices. The figures below were
// checked by hand against the log before the code existed, which is the point
// of keeping them as vectors — the ledger used to report zero for all of this.
func execution(t *testing.T, side market.Side, qty, price string, taker bool) (float64, float64) {
	t.Helper()
	g := &Guard{fees: config.Fees{MakerBps: 1, TakerBps: 4}}
	e := g.execution(trading.Trade{
		Side:    side,
		Qty:     money.MustParseQty(qty),
		Price:   money.MustParsePrice(price),
		IsTaker: taker,
	})
	cash, _ := e.CashUSD.Float64()
	fee, _ := e.FeeUSD.Float64()
	return cash, fee
}

func TestEpisodeOfElevenOhNine(t *testing.T) {
	var cash, fees float64
	for _, f := range []struct {
		side       market.Side
		qty, price string
		taker      bool
	}{
		{market.Sell, "0.061", "4330.1", false}, // quotes hit, we were the maker
		{market.Sell, "0.029", "4330.6", false},
		{market.Buy, "0.09", "4330.5", true}, // closed at market
	} {
		c, fee := execution(t, f.side, f.qty, f.price, f.taker)
		cash += c
		fees += fee
	}

	require.InDelta(t, -0.0215, cash, 0.001, "sold at 4330.26 on average, bought back at 4330.5")
	require.InDelta(t, 0.1949, fees, 0.001, "maker on both quotes, taker on the close")
	require.InDelta(t, -0.2164, cash-fees, 0.001, "the episode cost, and the fee is 90% of it")
}

func TestEpisodeOfTwelveTwentyNine(t *testing.T) {
	var cash, fees float64
	for _, f := range []struct {
		side       market.Side
		qty, price string
		taker      bool
	}{
		{market.Buy, "0.061", "4315.4", false},
		{market.Buy, "0.009", "4315.4", false},
		{market.Sell, "0.07", "4314.4", true},
	} {
		c, fee := execution(t, f.side, f.qty, f.price, f.taker)
		cash += c
		fees += fee
	}

	require.InDelta(t, -0.0700, cash, 0.001, "bought at 4315.4, sold a dollar lower")
	require.InDelta(t, 0.1510, fees, 0.001)

	// The cost model priced this episode at 0.162 and it actually cost 0.221.
	// Understating by a third is what the ledger has been protecting on.
	require.InDelta(t, -0.2210, cash-fees, 0.001)
	require.Less(t, 0.1616, -(cash - fees), "the model understates the real cost")
}

// A fee the exchange does send is preferred: the computation replaces a missing
// field, not the exchange's answer.
func TestReportedFeeWins(t *testing.T) {
	g := &Guard{fees: config.Fees{MakerBps: 1, TakerBps: 4}}
	e := g.execution(trading.Trade{
		Side:  market.Sell,
		Qty:   money.MustParseQty("1"),
		Price: money.MustParsePrice("100"),
		Fee:   decimal.NewFromFloat(0.5),
	})
	fee, _ := e.FeeUSD.Float64()
	require.InDelta(t, 0.5, fee, 1e-9)
}
