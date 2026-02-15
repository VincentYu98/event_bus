package eventbus

import "context"

// Subscribe registers a handler for events of type T.
// The handler receives both local publishes and remote messages via Transport.
func Subscribe[T Event](bus *Bus, handler func(ctx context.Context, ev T) error) {
	var zero T
	topic := zero.Topic()

	// Local handler: type-assert the in-process Go value.
	bus.addLocalHandler(topic, localHandler{
		fn: func(ctx context.Context, v any) error {
			return handler(ctx, v.(T))
		},
	})

	// Remote handler: deserialize payload into T using the Bus's Codec.
	bus.addRemoteHandler(topic, remoteHandler{
		fn: func(ctx context.Context, payload []byte) error {
			var ev T
			if err := bus.codec.Unmarshal(payload, &ev); err != nil {
				return err
			}
			return handler(ctx, ev)
		},
	})
}

// SubscribeLocal registers a handler for events of type T,
// but only for events published locally (not from Transport).
func SubscribeLocal[T Event](bus *Bus, handler func(ctx context.Context, ev T) error) {
	var zero T
	topic := zero.Topic()

	bus.addLocalHandler(topic, localHandler{
		fn: func(ctx context.Context, v any) error {
			return handler(ctx, v.(T))
		},
	})
}
