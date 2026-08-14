package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The venue quotes leverage in query_position_config and does not say so
// anywhere. A plain int field made every read fail, which cost the account its
// configured leverage without costing anyone an obvious error (PLAN.md §12.5).
func TestPositionConfigAcceptsLeverageInBothForms(t *testing.T) {
	for name, raw := range map[string]string{
		"quoted, as the venue actually sends it": `{"symbol":"XAU-USD","leverage":"40","margin_mode":"isolated"}`,
		"bare, as the documentation shows it":    `{"symbol":"XAU-USD","leverage":40,"margin_mode":"isolated"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var cfg PositionConfig
			require.NoError(t, json.Unmarshal([]byte(raw), &cfg))
			require.Equal(t, "XAU-USD", cfg.Symbol)
			require.EqualValues(t, 40, cfg.Leverage)
			require.Equal(t, "isolated", cfg.MarginMode)
		})
	}
}

func TestIntRejectsWhatIsNotAnInteger(t *testing.T) {
	for name, raw := range map[string]string{
		"quoted decimal": `"9.49"`,
		"empty string":   `""`,
		"words":          `"forty"`,
		"object":         `{}`,
		"null-ish array": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			var got Int
			require.Error(t, json.Unmarshal([]byte(raw), &got))
		})
	}
}

func TestIntToleratesSurroundingSpace(t *testing.T) {
	var got Int
	require.NoError(t, json.Unmarshal([]byte(`" 40 "`), &got))
	require.EqualValues(t, 40, got)
}

func TestIntAcceptsNegativeAndZero(t *testing.T) {
	var quoted, bare Int
	require.NoError(t, json.Unmarshal([]byte(`"-1"`), &quoted))
	require.EqualValues(t, -1, quoted)
	require.NoError(t, json.Unmarshal([]byte(`0`), &bare))
	require.EqualValues(t, 0, bare)
}
