package reward

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShippedWindowMatchesTheVenueSession(t *testing.T) {
	w, err := ParseWindow("Fri 21:00", "Sun 22:00")
	require.NoError(t, err)
	s := Schedule{Weekly: w}

	for name, tc := range map[string]struct {
		at     time.Time
		closed bool
	}{
		"пятница 20:59 UTC — рынок ещё открыт": {time.Date(2026, 8, 14, 20, 59, 0, 0, time.UTC), false},
		"пятница 21:00 UTC — закрытие":         {time.Date(2026, 8, 14, 21, 0, 0, 0, time.UTC), true},
		"суббота полдень":                      {time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC), true},
		"воскресенье 21:59 UTC — ещё закрыт":   {time.Date(2026, 8, 16, 21, 59, 0, 0, time.UTC), true},
		"воскресенье 22:00 UTC — открытие":     {time.Date(2026, 8, 16, 22, 0, 0, 0, time.UTC), false},
		"понедельник полдень":                  {time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.closed, s.Closed(tc.at))
		})
	}
}
