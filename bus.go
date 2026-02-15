package eventbus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
)

// localHandler holds a handler that receives in-process Go values.
type localHandler struct {
	fn func(ctx context.Context, v any) error
}

// remoteHandler holds a handler that deserializes from raw bytes.
type remoteHandler struct {
	fn func(ctx context.Context, payload []byte) error
}

// envelope is the wire format sent through a Transport.
// It is always serialized with encoding/json (not the user Codec).
type envelope struct {
	Origin  string          `json:"origin"`
	Payload json.RawMessage `json:"payload"`
}

// Bus is the central event dispatcher.
type Bus struct {
	id           string
	codec        Codec
	transport    Transport
	errorHandler ErrorHandler

	mu             sync.RWMutex
	localHandlers  map[string][]localHandler
	remoteHandlers map[string][]remoteHandler
	subscribed     map[string]bool // tracks topics already subscribed on Transport
}

// New creates a new Bus with the given options.
func New(opts ...Option) *Bus {
	b := &Bus{
		id:             newID(),
		codec:          JSONCodec{},
		localHandlers:  make(map[string][]localHandler),
		remoteHandlers: make(map[string][]remoteHandler),
		subscribed:     make(map[string]bool),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// ID returns the unique identifier of this Bus instance.
func (b *Bus) ID() string {
	return b.id
}

// Close shuts down the Bus's Transport (if any).
func (b *Bus) Close() error {
	if b.transport != nil {
		return b.transport.Close()
	}
	return nil
}

// addLocalHandler appends a local handler for the given topic.
func (b *Bus) addLocalHandler(topic string, h localHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.localHandlers[topic] = append(b.localHandlers[topic], h)
}

// addRemoteHandler appends a remote handler for the given topic.
// On the first remote handler for a topic, it subscribes on the Transport.
func (b *Bus) addRemoteHandler(topic string, h remoteHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remoteHandlers[topic] = append(b.remoteHandlers[topic], h)

	if b.transport != nil && !b.subscribed[topic] {
		b.subscribed[topic] = true
		t := b.transport
		id := b.id
		// Subscribe callback is invoked by the Transport for each message.
		_ = t.Subscribe(topic, func(ctx context.Context, data []byte) {
			var env envelope
			if err := json.Unmarshal(data, &env); err != nil {
				return
			}
			// Skip messages originating from this Bus (dedup).
			if env.Origin == id {
				return
			}
			b.mu.RLock()
			handlers := make([]remoteHandler, len(b.remoteHandlers[topic]))
			copy(handlers, b.remoteHandlers[topic])
			b.mu.RUnlock()

			for _, rh := range handlers {
				if err := rh.fn(ctx, []byte(env.Payload)); err != nil {
					b.mu.RLock()
					eh := b.errorHandler
					b.mu.RUnlock()
					if eh != nil {
						eh(err)
					}
				}
			}
		})
	}
}

// getLocalHandlers returns a snapshot of local handlers for the given topic.
func (b *Bus) getLocalHandlers(topic string) []localHandler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	handlers := make([]localHandler, len(b.localHandlers[topic]))
	copy(handlers, b.localHandlers[topic])
	return handlers
}

// newID generates a random UUID-like identifier.
func newID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
