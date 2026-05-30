// Package notifications owns the agent-side task.notification event fanout.
package notifications

import (
	"context"
	"sync"
	"time"
)

// Notification is the stable payload emitted as a task.notification frame.
type Notification struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Severity  string         `json:"severity,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Data      map[string]any `json:"data,omitempty"`
}

// Sink receives notifications from background modules.
type Sink interface {
	Publish(context.Context, Notification) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(context.Context, Notification) error

func (f SinkFunc) Publish(ctx context.Context, n Notification) error {
	return f(ctx, n)
}

// Hub fans notifications out to active server connections.
type Hub struct {
	mu   sync.Mutex
	next int64
	subs map[int64]func(Notification)
}

func NewHub() *Hub {
	return &Hub{subs: make(map[int64]func(Notification))}
}

func (h *Hub) Subscribe(fn func(Notification)) func() {
	h.mu.Lock()
	h.next++
	id := h.next
	h.subs[id] = fn
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(_ context.Context, n Notification) error {
	h.mu.Lock()
	subs := make([]func(Notification), 0, len(h.subs))
	for _, fn := range h.subs {
		subs = append(subs, fn)
	}
	h.mu.Unlock()
	for _, fn := range subs {
		fn(n)
	}
	return nil
}
