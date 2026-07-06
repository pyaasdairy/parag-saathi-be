// Package eventbus is the in-process pub/sub seam between modules
// (e.g. settlement executed → notifications module queues the SMS).
// It deliberately mirrors a message-broker API: when the platform splits
// into microservices, this interface is re-implemented over Kafka/NATS
// (blueprint §15) without touching module code.
package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Topics published across module seams.
const (
	TopicPourRecorded       = "pour.recorded"
	TopicInvoiceIssued      = "invoice.issued"
	TopicGateBlocked        = "qc.gate_blocked"
	TopicSettlementExecuted = "settlement.executed"
	TopicPayoutCredited     = "payout.credited"
	TopicMVUDispatched      = "mvu.dispatched"
)

// Handler consumes a published event. Payload types are documented per topic
// by the publishing module.
type Handler func(ctx context.Context, topic string, payload any)

// Bus is a minimal async pub/sub.
type Bus struct {
	log *slog.Logger

	mu   sync.RWMutex
	subs map[string][]Handler
}

// New builds an empty bus.
func New(log *slog.Logger) *Bus {
	return &Bus{log: log, subs: make(map[string][]Handler)}
}

// Subscribe registers a handler for a topic. Call during module wiring only
// (not concurrently with Publish traffic).
func (b *Bus) Subscribe(topic string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], h)
}

// Publish dispatches asynchronously: each handler runs on its own goroutine
// with a bounded context and panic isolation, so a slow or broken subscriber
// can never stall the request path.
func (b *Bus) Publish(topic string, payload any) {
	b.mu.RLock()
	handlers := b.subs[topic]
	b.mu.RUnlock()

	for _, h := range handlers {
		h := h
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			defer func() {
				if rec := recover(); rec != nil {
					b.log.Error("eventbus handler panic",
						slog.String("topic", topic), slog.Any("panic", rec))
				}
			}()
			h(ctx, topic, payload)
		}()
	}
}
