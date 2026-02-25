package eventbus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrBusClosed is returned when operating on a closed Bus.
var ErrBusClosed = errors.New("eventbus: bus closed")

// Subscription represents a registered event handler.
// Call Close to remove the handler from the Bus.
type Subscription struct {
	bus      *Bus
	topic    string
	localID  uint64
	remoteID uint64 // 0 if SubscribeLocal
}

// Close removes the handler from the Bus.
// It is safe to call multiple times or on a nil Subscription.
func (s *Subscription) Close() {
	if s == nil || s.bus == nil {
		return
	}
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	b.localHandlers[s.topic] = removeLocalByID(b.localHandlers[s.topic], s.localID)
	if s.remoteID > 0 {
		b.remoteHandlers[s.topic] = removeRemoteByID(b.remoteHandlers[s.topic], s.remoteID)
	}
	s.bus = nil
}

// localHandler holds a handler that receives in-process Go values.
type localHandler struct {
	id uint64
	fn func(ctx context.Context, v any) error
}

// remoteHandler holds a handler that deserializes from raw bytes.
type remoteHandler struct {
	id uint64
	fn func(ctx context.Context, payload []byte) error
}

// envelope is the wire format sent through a Transport.
// Payload is []byte so binary codecs (Protobuf, MsgPack, etc.) work;
// encoding/json automatically base64-encodes/decodes []byte fields.
type envelope struct {
	Origin  string `json:"origin"`
	Payload []byte `json:"payload"`
}

const defaultAsyncLimit = 4096

// Bus is the central event dispatcher.
type Bus struct {
	id           string
	codec        Codec
	transport    Transport
	errorHandler ErrorHandler
	closed       atomic.Bool
	nextID       atomic.Uint64
	asyncSem     chan struct{}

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
		asyncSem:       make(chan struct{}, defaultAsyncLimit),
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

// Close shuts down the Bus. After Close, new Subscribe and Publish calls
// will be rejected. In-flight asynchronous publishes may fail gracefully.
func (b *Bus) Close() error {
	b.closed.Store(true)
	if b.transport != nil {
		return b.transport.Close()
	}
	return nil
}

// addLocalHandler appends a local handler for the given topic and returns its ID.
func (b *Bus) addLocalHandler(topic string, h localHandler) uint64 {
	id := b.nextID.Add(1)
	h.id = id
	b.mu.Lock()
	defer b.mu.Unlock()
	b.localHandlers[topic] = append(b.localHandlers[topic], h)
	return id
}

// addRemoteHandler appends a remote handler for the given topic and returns its ID.
// On the first remote handler for a topic, it subscribes on the Transport.
// The Bus lock is released before calling transport.Subscribe to avoid deadlock.
func (b *Bus) addRemoteHandler(topic string, h remoteHandler) uint64 {
	id := b.nextID.Add(1)
	h.id = id

	b.mu.Lock()
	b.remoteHandlers[topic] = append(b.remoteHandlers[topic], h)

	needSubscribe := b.transport != nil && !b.subscribed[topic]
	var t Transport
	var busID string
	if needSubscribe {
		b.subscribed[topic] = true // prevent concurrent double-subscribe
		t = b.transport
		busID = b.id
	}
	b.mu.Unlock()

	if !needSubscribe {
		return id
	}

	// Subscribe on Transport without holding the Bus lock.
	err := t.Subscribe(topic, func(ctx context.Context, data []byte) {
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			b.reportError(fmt.Errorf("eventbus: unmarshal envelope: %w", err))
			return
		}
		// Skip messages originating from this Bus (dedup).
		if env.Origin == busID {
			return
		}
		b.mu.RLock()
		handlers := make([]remoteHandler, len(b.remoteHandlers[topic]))
		copy(handlers, b.remoteHandlers[topic])
		b.mu.RUnlock()

		for _, rh := range handlers {
			if err := rh.fn(ctx, env.Payload); err != nil {
				b.reportError(err)
			}
		}
	})

	if err != nil {
		// Revert subscribed flag so the next Subscribe call retries.
		b.mu.Lock()
		b.subscribed[topic] = false
		b.mu.Unlock()
		b.reportError(fmt.Errorf("eventbus: transport subscribe: %w", err))
	}

	return id
}

// getLocalHandlers returns a snapshot of local handlers for the given topic.
func (b *Bus) getLocalHandlers(topic string) []localHandler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	handlers := make([]localHandler, len(b.localHandlers[topic]))
	copy(handlers, b.localHandlers[topic])
	return handlers
}

// reportError calls the configured ErrorHandler, if any.
func (b *Bus) reportError(err error) {
	b.mu.RLock()
	eh := b.errorHandler
	b.mu.RUnlock()
	if eh != nil {
		eh(err)
	}
}

func removeLocalByID(handlers []localHandler, id uint64) []localHandler {
	for i, h := range handlers {
		if h.id == id {
			return append(handlers[:i], handlers[i+1:]...)
		}
	}
	return handlers
}

func removeRemoteByID(handlers []remoteHandler, id uint64) []remoteHandler {
	for i, h := range handlers {
		if h.id == id {
			return append(handlers[:i], handlers[i+1:]...)
		}
	}
	return handlers
}

// newID generates a random UUID-like identifier.
func newID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
