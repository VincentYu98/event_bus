package eventbus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBusClosed is returned when operating on a closed Bus.
var ErrBusClosed = errors.New("eventbus: bus closed")

const (
	defaultAsyncLimit   = 4096
	maxSubscribeRetries = 3
)

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

// Bus is the central event dispatcher.
type Bus struct {
	id           string
	codec        Codec
	transport    Transport
	errorHandler ErrorHandler
	nextID       atomic.Uint64
	asyncSem     chan struct{}

	mu             sync.RWMutex
	closed         bool
	done           chan struct{}
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
		done:           make(chan struct{}),
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

// Close shuts down the Bus. After Close returns, new Subscribe and Publish
// calls are guaranteed to be rejected. In-flight operations may complete
// or fail gracefully.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.done)
	b.mu.Unlock()

	if b.transport != nil {
		return b.transport.Close()
	}
	return nil
}

// addLocalHandler appends a local handler for the given topic and returns its ID.
// Returns ErrBusClosed if the Bus has been closed.
func (b *Bus) addLocalHandler(topic string, h localHandler) (uint64, error) {
	id := b.nextID.Add(1)
	h.id = id
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrBusClosed
	}
	b.localHandlers[topic] = append(b.localHandlers[topic], h)
	return id, nil
}

// addRemoteHandler appends a remote handler for the given topic and returns its ID.
// On the first remote handler for a topic, it subscribes on the Transport with retry.
// The Bus lock is released before calling transport.Subscribe to avoid deadlock.
// Returns ErrBusClosed if the Bus has been closed.
func (b *Bus) addRemoteHandler(topic string, h remoteHandler) (uint64, error) {
	id := b.nextID.Add(1)
	h.id = id

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, ErrBusClosed
	}
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
		return id, nil
	}

	// Subscribe on Transport without holding the Bus lock, with retry.
	callback := func(ctx context.Context, data []byte) {
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
		if b.closed {
			b.mu.RUnlock()
			return
		}
		handlers := make([]remoteHandler, len(b.remoteHandlers[topic]))
		copy(handlers, b.remoteHandlers[topic])
		b.mu.RUnlock()

		for _, rh := range handlers {
			if err := rh.fn(ctx, env.Payload); err != nil {
				b.reportError(err)
			}
		}
	}

	err := b.subscribeWithRetry(t, topic, callback)
	if err != nil {
		// Rollback: revert subscribed flag and remove the handler.
		b.mu.Lock()
		b.subscribed[topic] = false
		b.remoteHandlers[topic] = removeRemoteByID(b.remoteHandlers[topic], id)
		b.mu.Unlock()

		wrappedErr := fmt.Errorf("eventbus: transport subscribe: %w", err)
		b.reportError(wrappedErr)
		return 0, wrappedErr
	}

	return id, nil
}

// subscribeWithRetry attempts transport.Subscribe up to maxSubscribeRetries times
// with exponential backoff (20ms, 40ms). Aborts early if the Bus is closed.
func (b *Bus) subscribeWithRetry(t Transport, topic string, handler func(ctx context.Context, data []byte)) error {
	var err error
	for attempt := 0; attempt < maxSubscribeRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt)) * 10 * time.Millisecond
			select {
			case <-time.After(delay):
			case <-b.done:
				return ErrBusClosed
			}
		}
		if err = t.Subscribe(topic, handler); err == nil {
			return nil
		}
	}
	return err
}

// getLocalHandlers returns a snapshot of local handlers for the given topic.
// Returns ErrBusClosed if the Bus has been closed (checked under RLock
// for strong consistency with Close).
func (b *Bus) getLocalHandlers(topic string) ([]localHandler, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, ErrBusClosed
	}
	handlers := make([]localHandler, len(b.localHandlers[topic]))
	copy(handlers, b.localHandlers[topic])
	return handlers, nil
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
