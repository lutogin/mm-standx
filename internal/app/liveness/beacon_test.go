package liveness

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
)

var epoch = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func TestATickingActorIsAlive(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD"}, 5*time.Second, epoch)

	now := epoch
	for range 100 {
		now = now.Add(250 * time.Millisecond)
		b.Beat("XAU-USD", now)
	}

	_, _, stale := b.Stalest(now)
	require.False(t, stale, "an actor that keeps ticking is alive")
}

// Ровно тот отказ 13 августа: процесс жив, отвечает на healthcheck, а актор
// стоит. Признак жизни обязан следовать за актором, а не за процессом.
func TestAWedgedActorGoesStaleWhileTheProcessLives(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD"}, 5*time.Second, epoch)
	b.Beat("XAU-USD", epoch.Add(time.Second))

	symbol, silent, stale := b.Stalest(epoch.Add(40 * time.Minute))
	require.True(t, stale)
	require.EqualValues(t, "XAU-USD", symbol)
	require.Equal(t, 39*time.Minute+59*time.Second, silent)
}

// Актор, не поднявшийся ни разу, обязан считаться молчащим — иначе процесс без
// торговли выглядит здоровым бессрочно. PLAN.md §13: сторож не видел
// инструментов, от которых данные не приходили ни разу.
func TestAnActorThatNeverStartedIsStaleToo(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD"}, 5*time.Second, epoch)

	_, _, fresh := b.Stalest(epoch.Add(time.Second))
	require.False(t, fresh, "there is a grace period of exactly the threshold")

	symbol, _, stale := b.Stalest(epoch.Add(time.Minute))
	require.True(t, stale, "and it does not extend past it")
	require.EqualValues(t, "XAU-USD", symbol)
}

// Одного молчащего достаточно: торговля инструментом, чей актор мёртв,
// не становится безопаснее оттого, что соседний жив.
func TestOneSilentActorMakesTheWholeProcessStale(t *testing.T) {
	now := epoch.Add(time.Minute)
	b := New([]market.Symbol{"XAU-USD", "BTC-USD"}, 5*time.Second, epoch)
	b.Beat("XAU-USD", now)

	symbol, _, stale := b.Stalest(now)
	require.True(t, stale)
	require.EqualValues(t, "BTC-USD", symbol, "the loudest silence wins")
}

// Незарегистрированный актор не имеет права поддерживать процесс живым.
func TestAnUnexpectedSymbolCannotKeepTheProcessAlive(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD"}, 5*time.Second, epoch)

	now := epoch.Add(time.Minute)
	b.Beat("DOGE-USD", now)

	symbol, _, stale := b.Stalest(now)
	require.True(t, stale)
	require.EqualValues(t, "XAU-USD", symbol)
}

// Запоздавший такт не омолаживает отметку: иначе сообщение, застрявшее
// в очереди, задним числом объявляло бы актор живым.
func TestALateBeatDoesNotRejuvenate(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD"}, 5*time.Second, epoch)

	b.Beat("XAU-USD", epoch.Add(time.Minute))
	b.Beat("XAU-USD", epoch.Add(time.Second))

	_, _, stale := b.Stalest(epoch.Add(time.Minute + 2*time.Second))
	require.False(t, stale, "the late beat is ignored, the fresh one still counts")

	// Молчание отсчитывается от свежей отметки, а не от запоздавшей: иначе
	// было бы 10 минут, а не 10 секунд.
	_, silent, stale := b.Stalest(epoch.Add(time.Minute + 10*time.Second))
	require.True(t, stale)
	require.Equal(t, 10*time.Second, silent)
}

// Отметка ставится из горячего пути актора, а читают её сторож и HTTP.
func TestBeaconIsSafeUnderConcurrentUse(t *testing.T) {
	b := New([]market.Symbol{"XAU-USD", "BTC-USD"}, 5*time.Second, epoch)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			symbol := market.Symbol("XAU-USD")
			if i%2 == 1 {
				symbol = "BTC-USD"
			}
			for j := range 1000 {
				b.Beat(symbol, epoch.Add(time.Duration(j)*time.Millisecond))
				b.Stalest(epoch)
			}
		}()
	}
	wg.Wait()
}
