package calendar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Событие без валюты обязано проходить любой непустой список валют.
//
// Замер боевого счёта 28 августа 2026: в фиде стояло «16:15 All High Jackson
// Hole Symposium», фильтр [USD] его отбросил, сводка в 16:15 показывала
// «normal, nothing scheduled», и бот котировал сквозь открытие симпозиума.
// Минус 84.39 USD за день при пороге 60 и остановка по килл-свитчу.

func event(currency string, impact Impact) Event {
	return Event{
		Title:    "Jackson Hole Symposium",
		Currency: currency,
		Impact:   impact,
		At:       time.Date(2026, 8, 28, 16, 15, 0, 0, time.UTC),
	}
}

func TestAllCurrencyEventPassesACurrencyFilter(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	require.True(t, f.Match(event("All", ImpactHigh)),
		"an event with no single currency is not an event about a foreign one")
}

// Регистр и пробелы приходят из чужого фида и не должны решать ничего.
func TestAllCurrencyMatchIsCaseAndSpaceInsensitive(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	for _, spelling := range []string{"All", "ALL", "all", " All "} {
		require.True(t, f.Match(event(spelling, ImpactHigh)), spelling)
	}
}

// Уровень важности метка All не отменяет: отбор по нему остаётся прежним.
func TestAllCurrencyStillObeysTheImpactFilter(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	require.False(t, f.Match(event("All", ImpactLow)),
		"a low-impact symposium must not blank the book")
}

// Выключенный отбор обязан оставаться выключенным во всех ветках.
func TestEmptyCurrencyListAcceptsNothingIncludingAll(t *testing.T) {
	f := Filter{Impacts: []Impact{ImpactHigh}}
	require.False(t, f.Match(event("All", ImpactHigh)))
	require.False(t, f.Match(event("USD", ImpactHigh)))
}

// Чужая валюта по-прежнему отбрасывается — ради этого фильтр и написан.
func TestForeignCurrencyIsStillRejected(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	require.False(t, f.Match(event("GBP", ImpactHigh)))
}

// Select возвращает событие без валюты вместе с остальными, в порядке времени.
func TestSelectKeepsAllCurrencyEvents(t *testing.T) {
	f := Filter{Currencies: []string{"USD"}, Impacts: []Impact{ImpactHigh}}
	warsh := Event{
		Title: "Fed Chairman Warsh Speaks", Currency: "USD", Impact: ImpactHigh,
		At: time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC),
	}
	out := f.Select([]Event{event("All", ImpactHigh), warsh})
	require.Len(t, out, 2)
	require.Equal(t, "Fed Chairman Warsh Speaks", out[0].Title)
	require.Equal(t, "Jackson Hole Symposium", out[1].Title)
}
