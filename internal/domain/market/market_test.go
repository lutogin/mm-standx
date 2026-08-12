package market

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nd/mm-standx/internal/domain/money"
)

func testTime() time.Time { return time.Unix(1_754_897_028, 0).UTC() }

func spec(priceDecimals, qtyDecimals int32) SymbolSpec {
	return SymbolSpec{
		Symbol:            "BTC-USD",
		PriceTickDecimals: priceDecimals,
		QtyTickDecimals:   qtyDecimals,
		MinOrderQty:       money.MustParseQty("0.0001"),
		MaxOrderQty:       money.MustParseQty("100"),
	}
}

func TestPriceAtDistanceSidePlacement(t *testing.T) {
	mark := money.MustParsePrice("100000")

	bid := PriceAtDistance(mark, Buy, 8)
	ask := PriceAtDistance(mark, Sell, 8)

	require.True(t, bid.Lt(mark), "a buy order must rest below mark")
	require.True(t, ask.Gt(mark), "a sell order must rest above mark")
	require.Equal(t, "99920", bid.String())
	require.Equal(t, "100080", ask.String())
}

// Дистанция и цена обязаны быть взаимно обратимы: на этом соответствии держится
// весь контур управления, где решения принимаются в базисных пунктах,
// а на биржу уходят цены.
func TestDistanceRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		markFloat := rapid.Float64Range(0.01, 200_000).Draw(t, "mark")
		distance := money.Bps(rapid.Float64Range(0.1, 100).Draw(t, "distance"))
		side := Buy
		if rapid.Bool().Draw(t, "isSell") {
			side = Sell
		}

		mark := money.NewPrice(decimal.NewFromFloat(markFloat))
		price := PriceAtDistance(mark, side, distance)

		got, err := DistanceBps(mark, side, price)
		require.NoError(t, err)
		require.InDelta(t, float64(distance), float64(got), 1e-6)
	})
}

// Снэп к тику обязан только увеличивать дистанцию. Округление в сторону mark
// сделало бы ордер ближе к исполнению, чем рассчитала стратегия.
func TestSnapPriceNeverReducesDistance(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		markFloat := rapid.Float64Range(1, 200_000).Draw(t, "mark")
		distance := money.Bps(rapid.Float64Range(0.5, 50).Draw(t, "distance"))
		decimals := int32(rapid.IntRange(0, 6).Draw(t, "decimals"))
		side := Buy
		if rapid.Bool().Draw(t, "isSell") {
			side = Sell
		}

		s := spec(decimals, 4)
		mark := money.NewPrice(decimal.NewFromFloat(markFloat))
		snapped := s.SnapPrice(PriceAtDistance(mark, side, distance), side)

		got, err := DistanceBps(mark, side, snapped)
		require.NoError(t, err)
		require.GreaterOrEqual(t, float64(got), float64(distance)-1e-9,
			"snapping moved the order closer to mark: was %s, became %s", distance, got)
	})
}

func TestSnapPriceDirection(t *testing.T) {
	s := spec(2, 4)

	require.Equal(t, "99919.12", s.SnapPrice(money.MustParsePrice("99919.129"), Buy).String())
	require.Equal(t, "100080.13", s.SnapPrice(money.MustParsePrice("100080.121"), Sell).String())
}

// Отрицательная дистанция означает, что mark ушёл за цену ордера и тот стал
// маркетабельным — это сигнал немедленной опасности, а не просто малое число.
func TestDistanceIsNegativeWhenMarkCrossesOrder(t *testing.T) {
	mark := money.MustParsePrice("100000")

	d, err := DistanceBps(mark, Buy, money.MustParsePrice("100050"))
	require.NoError(t, err)
	require.Negative(t, float64(d))

	d, err = DistanceBps(mark, Sell, money.MustParsePrice("99950"))
	require.NoError(t, err)
	require.Negative(t, float64(d))
}

func TestDistanceRejectsNonPositiveMark(t *testing.T) {
	_, err := DistanceBps(money.MustParsePrice("0"), Buy, money.MustParsePrice("100"))
	require.Error(t, err)
}

func TestSnapQtyRoundsDown(t *testing.T) {
	s := spec(2, 3)
	require.Equal(t, "0.123", s.SnapQty(money.MustParseQty("0.123999")).String())
}

func TestValidateQty(t *testing.T) {
	s := spec(2, 4)

	require.NoError(t, s.ValidateQty(money.MustParseQty("1")))
	require.Error(t, s.ValidateQty(money.MustParseQty("0")), "zero size is not allowed")
	require.Error(t, s.ValidateQty(money.MustParseQty("0.00001")), "below the minimum")
	require.Error(t, s.ValidateQty(money.MustParseQty("101")), "above the maximum")
}

// Стакан приходит с негарантированным порядком уровней, поэтому конструктор
// обязан сортировать его сам.
func TestNewBookSortsLevels(t *testing.T) {
	book := NewBook("BTC-USD",
		[]Level{
			{Price: money.MustParsePrice("99900"), Qty: money.MustParseQty("1")},
			{Price: money.MustParsePrice("99950"), Qty: money.MustParseQty("2")},
			{Price: money.MustParsePrice("99920"), Qty: money.MustParseQty("3")},
		},
		[]Level{
			{Price: money.MustParsePrice("100100"), Qty: money.MustParseQty("1")},
			{Price: money.MustParsePrice("100010"), Qty: money.MustParseQty("2")},
		},
		testTime())

	bid, ok := book.BestBid()
	require.True(t, ok)
	require.Equal(t, "99950", bid.String(), "the best bid is the highest price")

	ask, ok := book.BestAsk()
	require.True(t, ok)
	require.Equal(t, "100010", ask.String(), "the best ask is the lowest price")
}

func TestEmptyBookHasNoBest(t *testing.T) {
	book := NewBook("BTC-USD", nil, nil, testTime())

	_, ok := book.BestBid()
	require.False(t, ok)
	_, ok = book.BestAsk()
	require.False(t, ok)
}

// Спецификация золота: десятицентовый шаг цены.
func goldSpec() SymbolSpec {
	return SymbolSpec{Symbol: "XAU-USD", PriceTickDecimals: 1, QtyTickDecimals: 3}
}

func twoSidedBook(bid, ask string) Book {
	return NewBook("XAU-USD",
		[]Level{{Price: money.MustParsePrice(bid), Qty: money.MustParseQty("1")}},
		[]Level{{Price: money.MustParsePrice(ask), Qty: money.MustParseQty("1")}},
		time.Time{})
}

func TestTickFollowsThePriceDecimals(t *testing.T) {
	require.Equal(t, "0.1", goldSpec().Tick().String())
	require.Equal(t, "0.01", SymbolSpec{PriceTickDecimals: 2}.Tick().String())
}

// В обычной обстановке котировка стоит глубже вершины на несколько базисных
// пунктов, и предел до неё не достаёт: зажим не имеет права трогать цену,
// которая и так безопасна.
func TestPostOnlyClampLeavesADeepQuoteAlone(t *testing.T) {
	book := twoSidedBook("4275", "4277")

	require.Equal(t, "4273", ClampPostOnly(money.MustParsePrice("4273"), Buy, book, goldSpec(), 2).String())
	require.Equal(t, "4279", ClampPostOnly(money.MustParsePrice("4279"), Sell, book, goldSpec(), 2).String())
}

// Ордер на самой вершине биржа примет, а рынок заберёт первым же движением.
// Между «не тейкер» и «не будет исполнен через мгновение» лежит расстояние,
// и зажим его отдаёт.
func TestPostOnlyClampPushesAQuoteOffTheTouch(t *testing.T) {
	book := twoSidedBook("4275", "4277")

	require.Equal(t, "4276.8", ClampPostOnly(money.MustParsePrice("4277"), Buy, book, goldSpec(), 2).String(),
		"a buy standing on the ask retreats by two ticks")
	require.Equal(t, "4275.2", ClampPostOnly(money.MustParsePrice("4275"), Sell, book, goldSpec(), 2).String(),
		"a sell standing on the bid retreats by two ticks")
}

// Смещение всегда направлено от mark price и никогда к нему.
func TestPostOnlyClampOnlyEverMovesAway(t *testing.T) {
	book := twoSidedBook("4275", "4277")
	spec := goldSpec()

	for _, price := range []string{"4270", "4276", "4276.9", "4280"} {
		p := money.MustParsePrice(price)
		require.True(t, ClampPostOnly(p, Buy, book, spec, 2).Lte(p), "buy %s", price)
		require.True(t, ClampPostOnly(p, Sell, book, spec, 2).Gte(p), "sell %s", price)
	}
}

func TestPostOnlyClampIsOffAtZeroBuffer(t *testing.T) {
	book := twoSidedBook("4275", "4277")
	price := money.MustParsePrice("4277")

	require.Equal(t, price, ClampPostOnly(price, Buy, book, goldSpec(), 0))
}

// Односторонний стакан не даёт от чего отступать: цена остаётся как есть,
// а пересечение ловит отдельная проверка.
func TestPostOnlyClampNeedsTheOpposingSide(t *testing.T) {
	oneSided := NewBook("XAU-USD",
		[]Level{{Price: money.MustParsePrice("4275"), Qty: money.MustParseQty("1")}},
		nil, time.Time{})
	price := money.MustParsePrice("4277")

	require.Equal(t, price, ClampPostOnly(price, Buy, oneSided, goldSpec(), 2))
}

// Зажатая цена обязана лежать на тике: биржа отвергнет цену между тиками,
// и округление в сторону смещения не отдаёт обратно только что отданный запас.
func TestPostOnlyClampSnapsAwayFromTheTouch(t *testing.T) {
	book := twoSidedBook("4275.05", "4277.05")
	spec := goldSpec()

	buy := ClampPostOnly(money.MustParsePrice("4277"), Buy, book, spec, 1)
	require.Equal(t, "4276.9", buy.String())
	require.False(t, book.WouldCross(Buy, buy))

	sell := ClampPostOnly(money.MustParsePrice("4275"), Sell, book, spec, 1)
	require.Equal(t, "4275.2", sell.String())
	require.False(t, book.WouldCross(Sell, sell))
}

func TestWouldCross(t *testing.T) {
	book := NewBook("BTC-USD",
		[]Level{{Price: money.MustParsePrice("99950"), Qty: money.MustParseQty("1")}},
		[]Level{{Price: money.MustParsePrice("100050"), Qty: money.MustParseQty("1")}},
		testTime())

	require.False(t, book.WouldCross(Buy, money.MustParsePrice("100040")))
	require.True(t, book.WouldCross(Buy, money.MustParsePrice("100050")), "buying at the ask price crosses the book")
	require.True(t, book.WouldCross(Buy, money.MustParsePrice("100060")))

	require.False(t, book.WouldCross(Sell, money.MustParsePrice("99960")))
	require.True(t, book.WouldCross(Sell, money.MustParsePrice("99950")), "selling at the bid price crosses the book")
	require.True(t, book.WouldCross(Sell, money.MustParsePrice("99940")))
}

// Котировка, рассчитанная от mark, не обязана быть безопасной относительно
// стакана: при широком спреде она может оказаться внутри него.
func TestQuoteInsideWideSpreadDoesNotCross(t *testing.T) {
	mark := money.MustParsePrice("100000")
	book := NewBook("BTC-USD",
		[]Level{{Price: money.MustParsePrice("99500"), Qty: money.MustParseQty("1")}},
		[]Level{{Price: money.MustParsePrice("100500"), Qty: money.MustParseQty("1")}},
		testTime())

	require.False(t, book.WouldCross(Buy, PriceAtDistance(mark, Buy, 8)))
	require.False(t, book.WouldCross(Sell, PriceAtDistance(mark, Sell, 8)))
}

func TestSideOpposite(t *testing.T) {
	require.Equal(t, Sell, Buy.Opposite())
	require.Equal(t, Buy, Sell.Opposite())
	require.True(t, Buy.Valid())
	require.False(t, Side("long").Valid())
}

// Spread only describes the top of the book. A book that is tight at the touch
// but empty behind it lets a small market order run several basis points deep
// and reach a quote that looked protected by the spread alone.
func TestDepthNotionalCountsOnlyWhatIsInsideTheBand(t *testing.T) {
	mid := money.MustParsePrice("100")
	level := func(p, q string) Level {
		return Level{Price: money.MustParsePrice(p), Qty: money.MustParseQty(q)}
	}
	book := NewBook("X-USD",
		[]Level{level("99.99", "1"), level("99.95", "2"), level("99.80", "100")},
		[]Level{level("100.01", "1"), level("100.05", "2"), level("100.20", "100")},
		time.Time{})

	// Ten basis points either side: the third level sits at 20 bps and is out.
	bid, _ := book.DepthNotional(mid, Buy, 10).Float64()
	ask, _ := book.DepthNotional(mid, Sell, 10).Float64()

	require.InDelta(t, 99.99*1+99.95*2, bid, 0.01)
	require.InDelta(t, 100.01*1+100.05*2, ask, 0.01)
}

func TestDepthNotionalIsZeroWithoutAReference(t *testing.T) {
	book := NewBook("X-USD",
		[]Level{{Price: money.MustParsePrice("99"), Qty: money.MustParseQty("1")}},
		nil, time.Time{})

	require.True(t, book.DepthNotional(money.Price{}, Buy, 10).IsZero())
	require.True(t, book.DepthNotional(money.MustParsePrice("100"), Buy, 0).IsZero())
}
