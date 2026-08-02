// Package events реализует типизированную in-process шину событий.
//
// Доставка синхронная и в порядке подписки. Это осознанный выбор: тесты становятся
// детерминированными, а поведение системы не зависит от планировщика горутин.
//
// Контракт обработчика: не блокироваться. Подписчик, которому нужна долгая работа,
// обязан положить событие в собственную очередь и немедленно вернуть управление —
// иначе он затормозит читателя WebSocket, который событие опубликовал.
package events

import (
	"context"
	"fmt"
	"sync"
)

// Event — доменный факт в прошедшем времени.
//
// Реализации обязаны быть структурами-значениями с методом на значении:
// Subscribe определяет имя топика по нулевому значению типа.
type Event interface {
	EventName() string
}

// PanicHandler вызывается, когда обработчик события паникует.
//
// Композиционный корень связывает его с kill-switch: паника обработчика означает
// неконсистентное состояние, и продолжать торговать в такой ситуации нельзя.
type PanicHandler func(topic string, recovered any)

type subscription struct {
	id      uint64
	invoke  func(context.Context, Event)
	subName string
}

// Bus — синхронный диспетчер событий.
type Bus struct {
	mu      sync.RWMutex
	topics  map[string][]subscription
	nextID  uint64
	onPanic PanicHandler
}

// New создаёт шину. onPanic может быть nil — тогда паники обработчиков поглощаются,
// что допустимо только в тестах.
func New(onPanic PanicHandler) *Bus {
	return &Bus{
		topics:  make(map[string][]subscription),
		onPanic: onPanic,
	}
}

// Publish синхронно доставляет событие всем подписчикам топика.
func Publish[E Event](b *Bus, ctx context.Context, e E) {
	topic := e.EventName()

	b.mu.RLock()
	subs := b.topics[topic]
	// Снимаем копию: обработчик имеет право подписаться или отписаться по ходу доставки.
	local := make([]subscription, len(subs))
	copy(local, subs)
	b.mu.RUnlock()

	for _, s := range local {
		b.dispatch(ctx, topic, s, e)
	}
}

func (b *Bus) dispatch(ctx context.Context, topic string, s subscription, e Event) {
	defer func() {
		if r := recover(); r != nil && b.onPanic != nil {
			b.onPanic(fmt.Sprintf("%s→%s", topic, s.subName), r)
		}
	}()
	s.invoke(ctx, e)
}

// Subscribe регистрирует обработчик события типа E и возвращает функцию отписки.
//
// name используется только в диагностике паник и должен указывать на подписчика,
// например "engine[BTC-USD]".
func Subscribe[E Event](b *Bus, name string, fn func(context.Context, E)) (unsubscribe func()) {
	var zero E
	topic := zero.EventName()

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.topics[topic] = append(b.topics[topic], subscription{
		id:      id,
		subName: name,
		invoke: func(ctx context.Context, e Event) {
			// Приведение безопасно: топик однозначно определяется типом события.
			fn(ctx, e.(E))
		},
	})
	b.mu.Unlock()

	return func() { b.unsubscribe(topic, id) }
}

func (b *Bus) unsubscribe(topic string, id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs := b.topics[topic]
	for i, s := range subs {
		if s.id == id {
			b.topics[topic] = append(subs[:i:i], subs[i+1:]...)
			return
		}
	}
}

// SubscriberCount возвращает число подписчиков топика. Используется в тестах и метриках.
func (b *Bus) SubscriberCount(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.topics[topic])
}
