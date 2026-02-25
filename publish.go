package eventbus

import (
	"context"
	"encoding/json"
	"errors"
)

// Publish sends an event of type T to all local handlers first,
// then publishes to the Transport (if configured).
// All local handlers are executed regardless of individual errors.
// Returns ErrBusClosed if the Bus has been closed.
func Publish[T Event](ctx context.Context, bus *Bus, event T) error {
	if bus.closed.Load() {
		return ErrBusClosed
	}

	topic := event.Topic()

	// Dispatch to local handlers.
	var errs []error
	for _, h := range bus.getLocalHandlers(topic) {
		if err := h.fn(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}

	// Publish to Transport.
	if bus.transport != nil {
		payload, err := bus.codec.Marshal(event)
		if err != nil {
			errs = append(errs, err)
		} else {
			env := envelope{
				Origin:  bus.id,
				Payload: payload,
			}
			data, err := json.Marshal(env)
			if err != nil {
				errs = append(errs, err)
			} else if err := bus.transport.Publish(ctx, topic, data); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// PublishAsync sends an event asynchronously.
// It acquires a semaphore slot (blocking if the concurrency limit is reached)
// and launches a goroutine. Use WithAsyncLimit to configure the limit.
// If an error occurs and an ErrorHandler is configured, it is called.
func PublishAsync[T Event](ctx context.Context, bus *Bus, event T) {
	if bus.closed.Load() {
		bus.reportError(ErrBusClosed)
		return
	}
	bus.asyncSem <- struct{}{} // backpressure: block if limit reached
	go func() {
		defer func() { <-bus.asyncSem }()
		if err := Publish(ctx, bus, event); err != nil {
			bus.reportError(err)
		}
	}()
}

// PublishLocal sends an event only to local handlers, bypassing the Transport.
// Returns ErrBusClosed if the Bus has been closed.
func PublishLocal[T Event](ctx context.Context, bus *Bus, event T) error {
	if bus.closed.Load() {
		return ErrBusClosed
	}

	topic := event.Topic()

	var errs []error
	for _, h := range bus.getLocalHandlers(topic) {
		if err := h.fn(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
