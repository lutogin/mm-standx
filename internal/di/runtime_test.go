package di

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
)

// Порог живости обязан покрывать худший синхронный поход актора в сеть.
//
// Актор ходит в сеть своей же горутиной, поэтому четыре повтора по
// requestTimeout с отступами — это молчание совершенно здорового актора.
// Порог ниже означал бы 503 на /healthz и перезапуск работающего бота,
// а перезапуск в середине часа стоит всего накопленного аптайма.
func TestLivenessThresholdCoversTheWorstBlockingCall(t *testing.T) {
	const (
		attempts = 4                       // rest.maxAttempts
		backoffs = 2100 * time.Millisecond // 200+400+800 плюс джиттер
	)

	for name, timeout := range map[string]time.Duration{
		"настроенный на проде": 5 * time.Second,
		"вдвое медленнее":      10 * time.Second,
		"агрессивный":          time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			worst := attempts*timeout + backoffs
			got := actorStale(config.Exchange{RequestTimeout: timeout})
			require.Greater(t, got, worst,
				"a healthy actor blocked on retries must not be declared dead")
		})
	}
}

// Ненастроенный таймаут не должен давать нулевой порог: он объявил бы мёртвым
// всё сразу.
func TestLivenessThresholdSurvivesAnUnsetTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, actorStale(config.Exchange{}))
}
