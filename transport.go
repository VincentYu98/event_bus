package eventbus

import (
	"context"
	"errors"
)

// Transport abstracts the underlying message delivery mechanism.
type Transport interface {
	// Publish sends raw bytes to the given topic.
	Publish(ctx context.Context, topic string, data []byte) error
	// Subscribe registers a handler for the given topic.
	Subscribe(topic string, handler func(ctx context.Context, data []byte)) error
	// Close shuts down the transport.
	Close() error
}

// ErrTransportClosed is returned when operating on a closed transport.
var ErrTransportClosed = errors.New("eventbus: transport closed")
