package eventbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test event types ---

type UserCreated struct {
	Name string `json:"name"`
}

func (UserCreated) Topic() string { return "user.created" }

type OrderPlaced struct {
	OrderID int `json:"order_id"`
}

func (OrderPlaced) Topic() string { return "order.placed" }

// --- tests ---

func TestNewDefault(t *testing.T) {
	bus := New()
	if bus.id == "" {
		t.Fatal("expected non-empty ID")
	}
	if _, ok := bus.codec.(JSONCodec); !ok {
		t.Fatal("expected default JSONCodec")
	}
	if bus.transport != nil {
		t.Fatal("expected nil transport by default")
	}
}

func TestPublishLocalSingleHandler(t *testing.T) {
	bus := New()
	var got UserCreated
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		got = ev
		return nil
	})

	err := PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "alice" {
		t.Fatalf("expected alice, got %s", got.Name)
	}
}

func TestPublishLocalMultipleHandlers(t *testing.T) {
	bus := New()
	var order []int

	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		order = append(order, 1)
		return nil
	})
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		order = append(order, 2)
		return nil
	})
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		order = append(order, 3)
		return nil
	})

	_ = PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "bob"})

	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("expected [1 2 3], got %v", order)
	}
}

func TestPublishLocalNoSubscribers(t *testing.T) {
	bus := New()
	err := PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "nobody"})
	if err != nil {
		t.Fatalf("expected no error for missing subscribers, got %v", err)
	}
}

func TestSubscribeLocalIgnoresRemote(t *testing.T) {
	mt := NewMemoryTransport()
	bus1 := New(WithTransport(mt))
	bus2 := New(WithTransport(mt))

	var called atomic.Int32
	// SubscribeLocal on bus2 should NOT receive messages from bus1.
	SubscribeLocal[UserCreated](bus2, func(ctx context.Context, ev UserCreated) error {
		called.Add(1)
		return nil
	})

	_ = Publish[UserCreated](context.Background(), bus1, UserCreated{Name: "remote"})

	// Give some time for potential (incorrect) delivery.
	time.Sleep(10 * time.Millisecond)

	if called.Load() != 0 {
		t.Fatal("SubscribeLocal handler should not receive remote messages")
	}
}

func TestCrossBusPublish(t *testing.T) {
	mt := NewMemoryTransport()
	bus1 := New(WithTransport(mt))
	bus2 := New(WithTransport(mt))

	var got UserCreated
	Subscribe[UserCreated](bus2, func(ctx context.Context, ev UserCreated) error {
		got = ev
		return nil
	})

	err := Publish[UserCreated](context.Background(), bus1, UserCreated{Name: "cross"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// MemoryTransport is synchronous, so the handler should have been called.
	if got.Name != "cross" {
		t.Fatalf("expected cross, got %s", got.Name)
	}
}

func TestSelfMessageDedup(t *testing.T) {
	mt := NewMemoryTransport()
	bus := New(WithTransport(mt))

	var count atomic.Int32
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		count.Add(1)
		return nil
	})

	_ = Publish[UserCreated](context.Background(), bus, UserCreated{Name: "self"})

	// Handler should be called exactly once (local), not twice (local + remote echo).
	if count.Load() != 1 {
		t.Fatalf("expected 1 call (deduped), got %d", count.Load())
	}
}

func TestHandlerErrorContinues(t *testing.T) {
	bus := New()

	errBoom := errors.New("boom")
	var secondCalled bool

	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		return errBoom
	})
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		secondCalled = true
		return nil
	})

	err := PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "err"})
	if !secondCalled {
		t.Fatal("second handler should have been called despite first handler error")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected error to contain errBoom, got %v", err)
	}
}

func TestPublishAsync(t *testing.T) {
	bus := New()

	var wg sync.WaitGroup
	wg.Add(1)

	var got UserCreated
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		got = ev
		wg.Done()
		return nil
	})

	PublishAsync[UserCreated](context.Background(), bus, UserCreated{Name: "async"})
	wg.Wait()

	if got.Name != "async" {
		t.Fatalf("expected async, got %s", got.Name)
	}
}

func TestPublishAsyncErrorHandler(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	var handledErr error
	bus := New(WithErrorHandler(func(err error) {
		handledErr = err
		wg.Done()
	}))

	errFail := errors.New("fail")
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		return errFail
	})

	PublishAsync[UserCreated](context.Background(), bus, UserCreated{Name: "errAsync"})
	wg.Wait()

	if !errors.Is(handledErr, errFail) {
		t.Fatalf("expected ErrorHandler to receive errFail, got %v", handledErr)
	}
}

func TestMultipleTopics(t *testing.T) {
	bus := New()

	var userCalled, orderCalled bool
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		userCalled = true
		return nil
	})
	Subscribe[OrderPlaced](bus, func(ctx context.Context, ev OrderPlaced) error {
		orderCalled = true
		return nil
	})

	_ = PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "x"})

	if !userCalled {
		t.Fatal("UserCreated handler should have been called")
	}
	if orderCalled {
		t.Fatal("OrderPlaced handler should NOT have been called")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	bus := New()

	var count atomic.Int64
	Subscribe[UserCreated](bus, func(ctx context.Context, ev UserCreated) error {
		count.Add(1)
		return nil
	})

	var wg sync.WaitGroup
	n := 100
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = PublishLocal[UserCreated](context.Background(), bus, UserCreated{Name: "concurrent"})
		}()
	}
	wg.Wait()

	if count.Load() != int64(n) {
		t.Fatalf("expected %d calls, got %d", n, count.Load())
	}
}

func TestClose(t *testing.T) {
	mt := NewMemoryTransport()
	bus := New(WithTransport(mt))

	if err := bus.Close(); err != nil {
		t.Fatalf("unexpected error on close: %v", err)
	}

	// Transport should be closed; publishing should fail.
	err := mt.Publish(context.Background(), "test", []byte("data"))
	if !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}
