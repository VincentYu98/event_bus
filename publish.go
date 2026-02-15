package eventbus

import (
	"context"
	"encoding/json"
	"errors"
)

// Publish sends an event of type T to all local handlers first,
// then publishes to the Transport (if configured).
// All local handlers are executed regardless of individual errors.
func Publish[T Event](ctx context.Context, bus *Bus, event T) error {
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
				Payload: json.RawMessage(payload),
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

// PublishAsync sends an event asynchronously in a new goroutine.
// If an error occurs and an ErrorHandler is configured, it is called.
func PublishAsync[T Event](ctx context.Context, bus *Bus, event T) {
	go func() {
		if err := Publish(ctx, bus, event); err != nil {
			bus.mu.RLock()
			eh := bus.errorHandler
			bus.mu.RUnlock()
			if eh != nil {
				eh(err)
			}
		}
	}()
}

// PublishLocal sends an event only to local handlers, bypassing the Transport.
func PublishLocal[T Event](ctx context.Context, bus *Bus, event T) error {
	topic := event.Topic()

	var errs []error
	for _, h := range bus.getLocalHandlers(topic) {
		if err := h.fn(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
