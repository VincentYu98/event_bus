package eventbus

import (
	"context"
	"fmt"
)

// Subscribe registers a handler for events of type T.
// The handler receives both local publishes and remote messages via Transport.
// Returns a Subscription that can be closed to remove the handler.
// Returns nil if the Bus is closed.
func Subscribe[T Event](bus *Bus, handler func(ctx context.Context, ev T) error) *Subscription {
	var zero T
	topic := zero.Topic()

	if bus.closed.Load() {
		bus.reportError(ErrBusClosed)
		return nil
	}

	// Local handler: type-assert the in-process Go value (safe, no panic).
	localID := bus.addLocalHandler(topic, localHandler{
		fn: func(ctx context.Context, v any) error {
			ev, ok := v.(T)
			if !ok {
				return fmt.Errorf("eventbus: type assertion failed: expected %T, got %T", zero, v)
			}
			return handler(ctx, ev)
		},
	})

	// Remote handler: deserialize payload into T using the Bus's Codec.
	remoteID := bus.addRemoteHandler(topic, remoteHandler{
		fn: func(ctx context.Context, payload []byte) error {
			var ev T
			if err := bus.codec.Unmarshal(payload, &ev); err != nil {
				return err
			}
			return handler(ctx, ev)
		},
	})

	return &Subscription{
		bus:      bus,
		topic:    topic,
		localID:  localID,
		remoteID: remoteID,
	}
}

// SubscribeLocal registers a handler for events of type T,
// but only for events published locally (not from Transport).
// Returns a Subscription that can be closed to remove the handler.
// Returns nil if the Bus is closed.
func SubscribeLocal[T Event](bus *Bus, handler func(ctx context.Context, ev T) error) *Subscription {
	var zero T
	topic := zero.Topic()

	if bus.closed.Load() {
		bus.reportError(ErrBusClosed)
		return nil
	}

	localID := bus.addLocalHandler(topic, localHandler{
		fn: func(ctx context.Context, v any) error {
			ev, ok := v.(T)
			if !ok {
				return fmt.Errorf("eventbus: type assertion failed: expected %T, got %T", zero, v)
			}
			return handler(ctx, ev)
		},
	})

	return &Subscription{
		bus:     bus,
		topic:   topic,
		localID: localID,
	}
}
