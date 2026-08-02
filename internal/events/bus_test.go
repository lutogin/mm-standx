package events

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type alpha struct{ N int }

func (alpha) EventName() string { return "test.alpha" }

type beta struct{ S string }

func (beta) EventName() string { return "test.beta" }

func TestPublishDeliversToSubscribers(t *testing.T) {
	bus := New(nil)

	var got []int
	Subscribe(bus, "collector", func(_ context.Context, e alpha) { got = append(got, e.N) })

	Publish(bus, context.Background(), alpha{N: 1})
	Publish(bus, context.Background(), alpha{N: 2})

	require.Equal(t, []int{1, 2}, got)
}

// Топик определяется типом события, поэтому подписчик одного типа не должен
// получать события другого — приведение внутри шины иначе паниковало бы.
func TestSubscribersAreIsolatedByType(t *testing.T) {
	bus := New(nil)

	var alphas, betas int
	Subscribe(bus, "a", func(context.Context, alpha) { alphas++ })
	Subscribe(bus, "b", func(context.Context, beta) { betas++ })

	Publish(bus, context.Background(), alpha{})
	Publish(bus, context.Background(), beta{})
	Publish(bus, context.Background(), beta{})

	require.Equal(t, 1, alphas)
	require.Equal(t, 2, betas)
}

func TestDeliveryFollowsSubscriptionOrder(t *testing.T) {
	bus := New(nil)

	var order []string
	Subscribe(bus, "first", func(context.Context, alpha) { order = append(order, "first") })
	Subscribe(bus, "second", func(context.Context, alpha) { order = append(order, "second") })
	Subscribe(bus, "third", func(context.Context, alpha) { order = append(order, "third") })

	Publish(bus, context.Background(), alpha{})

	require.Equal(t, []string{"first", "second", "third"}, order)
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := New(nil)

	var kept, dropped int
	Subscribe(bus, "kept", func(context.Context, alpha) { kept++ })
	unsubscribe := Subscribe(bus, "dropped", func(context.Context, alpha) { dropped++ })

	Publish(bus, context.Background(), alpha{})
	unsubscribe()
	Publish(bus, context.Background(), alpha{})

	require.Equal(t, 2, kept)
	require.Equal(t, 1, dropped)
	require.Equal(t, 1, bus.SubscriberCount(alpha{}.EventName()))
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	bus := New(nil)
	unsubscribe := Subscribe(bus, "sub", func(context.Context, alpha) {})

	unsubscribe()
	require.NotPanics(t, unsubscribe)
	require.Equal(t, 0, bus.SubscriberCount(alpha{}.EventName()))
}

// Паника одного обработчика не должна мешать остальным получить событие:
// иначе сбой в метриках лишил бы торговый контур данных.
func TestPanicIsIsolatedAndReported(t *testing.T) {
	var reportedTopic string
	var recovered any
	bus := New(func(topic string, r any) { reportedTopic, recovered = topic, r })

	var reached bool
	Subscribe(bus, "broken", func(context.Context, alpha) { panic("boom") })
	Subscribe(bus, "healthy", func(context.Context, alpha) { reached = true })

	require.NotPanics(t, func() { Publish(bus, context.Background(), alpha{}) })

	require.True(t, reached, "a healthy handler must still receive the event")
	require.Equal(t, "test.alpha→broken", reportedTopic)
	require.Equal(t, "boom", recovered)
}

// Подписка во время доставки не должна портить обход: список копируется
// до вызова обработчиков.
func TestSubscribeDuringDeliveryIsSafe(t *testing.T) {
	bus := New(nil)

	var late int
	Subscribe(bus, "spawner", func(context.Context, alpha) {
		Subscribe(bus, "late", func(context.Context, alpha) { late++ })
	})

	require.NotPanics(t, func() { Publish(bus, context.Background(), alpha{}) })
	require.Equal(t, 0, late, "a new subscriber must not receive the event already in flight")

	Publish(bus, context.Background(), alpha{})
	require.Positive(t, late)
}

func TestPublishWithoutSubscribersIsNoop(t *testing.T) {
	bus := New(nil)
	require.NotPanics(t, func() { Publish(bus, context.Background(), alpha{N: 7}) })
}

func TestConcurrentSubscribeAndPublish(t *testing.T) {
	bus := New(nil)

	var mu sync.Mutex
	var count int
	Subscribe(bus, "counter", func(context.Context, alpha) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			Publish(bus, context.Background(), alpha{})
		}()
		go func() {
			defer wg.Done()
			Subscribe(bus, "churn", func(context.Context, beta) {})()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 50, count)
}

func TestContextIsPassedThrough(t *testing.T) {
	bus := New(nil)
	type key struct{}

	var seen string
	Subscribe(bus, "reader", func(ctx context.Context, _ alpha) {
		seen, _ = ctx.Value(key{}).(string)
	})

	Publish(bus, context.WithValue(context.Background(), key{}, "value"), alpha{})
	require.Equal(t, "value", seen)
}
