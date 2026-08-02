package money

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Точность обязана переживать разбор и вывод без потерь: цена — не float,
// и округление здесь означало бы ордер не по той цене.
func TestParsePreservesPrecision(t *testing.T) {
	for _, s := range []string{"0.000000001", "121899.99", "99999999.123456789", "57.91"} {
		p, err := ParsePrice(s)
		require.NoError(t, err)
		require.Equal(t, s, p.String())
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	_, err := ParsePrice("не число")
	require.Error(t, err)
	_, err = ParseQty("")
	require.Error(t, err)
}

// Биржа отдаёт незаполненные числовые поля пустой строкой, поэтому декодер
// обязан трактовать её как ноль, а не падать.
func TestJSONRoundTrip(t *testing.T) {
	type payload struct {
		Price Price `json:"price"`
		Qty   Qty   `json:"qty"`
	}

	var got payload
	require.NoError(t, json.Unmarshal([]byte(`{"price":"121899.99","qty":"0.0601"}`), &got))
	require.Equal(t, "121899.99", got.Price.String())
	require.Equal(t, "0.0601", got.Qty.String())

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, `{"price":"121899.99","qty":"0.0601"}`, string(encoded))

	var empty payload
	require.NoError(t, json.Unmarshal([]byte(`{"price":"","qty":""}`), &empty))
	require.True(t, empty.Price.IsZero())
	require.True(t, empty.Qty.IsZero())
}

func TestSnapDirection(t *testing.T) {
	p := MustParsePrice("121899.996")
	require.Equal(t, "121899.99", p.SnapDown(2).String())
	require.Equal(t, "121900", p.SnapUp(2).String())

	// Округление вниз применяется и к количеству: превысить лимит размера хуже,
	// чем недобрать.
	require.Equal(t, "0.123", MustParseQty("0.1239").SnapDown(3).String())
}

func TestRelativeBps(t *testing.T) {
	base := MustParsePrice("100000")

	up, err := RelativeBps(base, MustParsePrice("100010"))
	require.NoError(t, err)
	require.InDelta(t, 1.0, float64(up), 1e-9)

	down, err := RelativeBps(base, MustParsePrice("99920"))
	require.NoError(t, err)
	require.InDelta(t, -8.0, float64(down), 1e-9)

	same, err := RelativeBps(base, base)
	require.NoError(t, err)
	require.Zero(t, float64(same))
}

func TestRelativeBpsRejectsNonPositiveBase(t *testing.T) {
	_, err := RelativeBps(MustParsePrice("0"), MustParsePrice("100"))
	require.Error(t, err)
	_, err = RelativeBps(MustParsePrice("-5"), MustParsePrice("100"))
	require.Error(t, err)
}

func TestBpsRatio(t *testing.T) {
	require.True(t, Bps(10).Ratio().Equal(decimal.NewFromFloat(0.001)))
	require.True(t, Bps(0).Ratio().IsZero())
	require.Equal(t, Bps(7.5), Bps(-7.5).Abs())
}

func TestNotional(t *testing.T) {
	value := Notional(MustParsePrice("62734.16"), MustParseQty("0.5"))
	require.Equal(t, "31367.08", value.String())

	// Короткая позиция задаётся отрицательным количеством, но её стоимость
	// положительна.
	short := Notional(MustParsePrice("100"), MustParseQty("-3"))
	require.Equal(t, "300", short.String())
}

func TestQtyForNotional(t *testing.T) {
	qty, err := QtyForNotional(decimal.NewFromInt(30000), MustParsePrice("60000"))
	require.NoError(t, err)
	require.Equal(t, "0.5", qty.String())

	_, err = QtyForNotional(decimal.NewFromInt(1), MustParsePrice("0"))
	require.Error(t, err)
}

func TestComparisons(t *testing.T) {
	low, high := MustParsePrice("100"), MustParsePrice("200")

	require.True(t, low.Lt(high))
	require.True(t, low.Lte(high))
	require.True(t, high.Gt(low))
	require.True(t, high.Gte(low))
	require.True(t, low.Lte(MustParsePrice("100")))
	require.Equal(t, 0, low.Cmp(MustParsePrice("100")))
}

func TestQtySignHelpers(t *testing.T) {
	short := MustParseQty("-1.5")
	require.True(t, short.IsNegative())
	require.Equal(t, "1.5", short.Abs().String())
	require.Equal(t, "1.5", short.Neg().String())
	require.True(t, MustParseQty("0").IsZero())
}
