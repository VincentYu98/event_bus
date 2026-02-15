package eventbus

// Event is the interface that all event types must implement.
// Topic returns the string key used for routing.
// IMPORTANT: Topic() must be a value receiver method so that
// var zero T works without a nil pointer dereference.
type Event interface {
	Topic() string
}
