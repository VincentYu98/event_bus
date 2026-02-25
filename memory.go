package eventbus

import (
	"context"
	"sync"
)

// MemoryTransport is an in-process Transport for testing and single-process use.
// Multiple Bus instances sharing the same MemoryTransport can communicate.
type MemoryTransport struct {
	mu       sync.RWMutex
	handlers map[string][]func(ctx context.Context, data []byte)
	closed   bool
}

// NewMemoryTransport creates a new MemoryTransport.
func NewMemoryTransport() *MemoryTransport {
	return &MemoryTransport{
		handlers: make(map[string][]func(ctx context.Context, data []byte)),
	}
}

// Publish sends data to all subscribers of the given topic synchronously.
// The data slice is copied to prevent caller buffer reuse issues.
func (m *MemoryTransport) Publish(ctx context.Context, topic string, data []byte) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ErrTransportClosed
	}
	handlers := make([]func(ctx context.Context, data []byte), len(m.handlers[topic]))
	copy(handlers, m.handlers[topic])
	m.mu.RUnlock()

	// Copy data per handler to prevent both caller reuse and cross-handler mutation.
	for _, h := range handlers {
		buf := make([]byte, len(data))
		copy(buf, data)
		h(ctx, buf)
	}
	return nil
}

// Subscribe registers a handler for the given topic.
func (m *MemoryTransport) Subscribe(topic string, handler func(ctx context.Context, data []byte)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrTransportClosed
	}
	m.handlers[topic] = append(m.handlers[topic], handler)
	return nil
}

// Close marks the transport as closed. Subsequent Publish/Subscribe calls return ErrTransportClosed.
func (m *MemoryTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
