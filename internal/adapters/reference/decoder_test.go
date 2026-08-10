package reference

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
)

func TestDecoderRecognisesBinance(t *testing.T) {
	d := NewDecoder()

	mid, ok := d.Mid([]byte(`{"e":"bookTicker","s":"XAUUSDT","b":"4357.80","B":"3","a":"4358.00","A":"5"}`))

	require.True(t, ok)
	require.Equal(t, "4357.9", mid.String())

	provider, symbol := d.Venue()
	require.Equal(t, config.ProviderBinanceFutures, provider)
	require.Equal(t, "XAUUSDT", symbol)
}

func TestDecoderRecognisesBybitAndFollowsDeltas(t *testing.T) {
	d := NewDecoder()

	mid, ok := d.Mid([]byte(`{"topic":"tickers.BTCUSDT","type":"snapshot",` +
		`"data":{"bid1Price":"100.00","ask1Price":"102.00"}}`))
	require.True(t, ok)
	require.Equal(t, "101", mid.String())

	mid, ok = d.Mid([]byte(`{"topic":"tickers.BTCUSDT","type":"delta","data":{"ask1Price":"104.00"}}`))
	require.True(t, ok)
	require.Equal(t, "102", mid.String())

	provider, symbol := d.Venue()
	require.Equal(t, config.ProviderBybitLinear, provider)
	require.Equal(t, "BTCUSDT", symbol)
}

// Пока площадка не распознана, декодер молчит: служебные кадры в начале записи
// не должны навязывать формат остальному потоку.
func TestDecoderSkipsFramesItCannotRecognise(t *testing.T) {
	d := NewDecoder()

	_, ok := d.Mid([]byte(`{"result":null,"id":1}`))
	require.False(t, ok)

	provider, _ := d.Venue()
	require.Empty(t, provider)

	_, ok = d.Mid([]byte(`{"s":"BTCUSDT","b":"1.0","B":"2","a":"1.2","A":"3"}`))
	require.True(t, ok)
}

// Ошибка, ради которой поля объёмов вообще объявлены: разбор JSON в Go
// принимает совпадение без учёта регистра, и "B" затирал бы "b".
func TestDecoderDoesNotConfuseQuantityWithPrice(t *testing.T) {
	d := NewDecoder()

	mid, ok := d.Mid([]byte(`{"s":"BTCUSDT","b":"100.00","B":"31.21","a":"102.00","A":"40.66"}`))

	require.True(t, ok)
	require.Equal(t, "101", mid.String(), "the mid must come from prices, not from sizes")
}
