package news

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/calendar"
)

// Настоящий кадр издателя, снятый 10 августа.
const sample = `[
  {"title":"Bank Lending y/y","country":"JPY","date":"2026-08-09T19:50:00-04:00",
   "impact":"Low","forecast":"5.7%","previous":"5.7%"},
  {"title":"CPI m/m","country":"USD","date":"2026-08-12T08:30:00-04:00",
   "impact":"High","forecast":"0.3%","previous":"0.2%"},
  {"title":"Bank Holiday","country":"JPY","date":"2026-08-11T00:00:00-04:00",
   "impact":"Holiday","forecast":"","previous":""}
]`

func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.Header.Get("User-Agent"), "the publisher rejects requests without one")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client())
}

func TestEventsParseTheWeeklyFeed(t *testing.T) {
	got, err := serve(t, http.StatusOK, sample).Events(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 3)

	cpi := got[1]
	require.Equal(t, "CPI m/m", cpi.Title)
	require.Equal(t, "USD", cpi.Currency)
	require.Equal(t, calendar.ImpactHigh, cpi.Impact)
	// 08:30 в зоне издателя — это 12:30 UTC. Смещение разбирается, а не
	// предполагается: иначе блэкаут разъехался бы с публикацией при переходе
	// на летнее время.
	require.Equal(t, time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC), cpi.At)
}

func TestImpactIsNormalised(t *testing.T) {
	got, err := serve(t, http.StatusOK, sample).Events(context.Background())

	require.NoError(t, err)
	require.Equal(t, calendar.ImpactLow, got[0].Impact)
	require.Equal(t, calendar.ImpactHoliday, got[2].Impact)
}

// Строка, которую не удалось разобрать, пропускается. Отказ от всего календаря
// из-за одной такой превратил бы косметическое изменение у издателя в слепое
// пятно перед ближайшей публикацией.
func TestMalformedRowsAreSkippedNotFatal(t *testing.T) {
	body := `[
	  {"title":"broken","country":"USD","date":"not a date","impact":"High"},
	  {"title":"CPI m/m","country":"USD","date":"2026-08-12T08:30:00-04:00","impact":"High"}
	]`

	got, err := serve(t, http.StatusOK, body).Events(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "CPI m/m", got[0].Title)
}

func TestNonOKStatusIsAnError(t *testing.T) {
	_, err := serve(t, http.StatusServiceUnavailable, "").Events(context.Background())

	require.ErrorContains(t, err, "status 503")
}

func TestMalformedBodyIsAnError(t *testing.T) {
	_, err := serve(t, http.StatusOK, "<html>maintenance</html>").Events(context.Background())

	require.ErrorContains(t, err, "decode calendar")
}

func TestEmptyURLFallsBackToThePublishedFeed(t *testing.T) {
	require.Equal(t, DefaultURL, New("", http.DefaultClient).url)
}
