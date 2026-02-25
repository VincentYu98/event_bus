package eventbus

import (
	"context"
	"encoding/json"
	"errors"
)

// Publish sends an event of type T to all local handlers first,
// then publishes to the Transport (if configured).
// All local handlers are executed regardless of individual errors.
// Returns ErrBusClosed if the Bus has been closed (checked under lock
// for strong consistency with Close).
func Publish[T Event](ctx context.Context, bus *Bus, event T) error {
	topic := event.Topic()

	// getLocalHandlers checks closed under RLock — no TOCTOU gap.
	handlers, err := bus.getLocalHandlers(topic)
	if err != nil {
		return err
	}

	// Dispatch to local handlers.
	var errs []error
	for _, h := range handlers {
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
// It acquires a semaphore slot and launches a goroutine.
// Uses select with the Bus's done channel so it never blocks on a closed Bus.
// If an error occurs and an ErrorHandler is configured, it is called.
func PublishAsync[T Event](ctx context.Context, bus *Bus, event T) {
	select {
	case bus.asyncSem <- struct{}{}:
		go func() {
			defer func() { <-bus.asyncSem }()
			if err := Publish(ctx, bus, event); err != nil {
				bus.reportError(err)
			}
		}()
	case <-bus.done:
		bus.reportError(ErrBusClosed)
	}
}

// PublishLocal sends an event only to local handlers, bypassing the Transport.
// Returns ErrBusClosed if the Bus has been closed (checked under lock
// for strong consistency with Close).
func PublishLocal[T Event](ctx context.Context, bus *Bus, event T) error {
	topic := event.Topic()

	handlers, err := bus.getLocalHandlers(topic)
	if err != nil {
		return err
	}

	var errs []error
	for _, h := range handlers {
		if err := h.fn(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
