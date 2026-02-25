package eventbus

// ErrorHandler is called when an asynchronous operation encounters an error.
type ErrorHandler func(err error)

// Option configures a Bus.
type Option func(*Bus)

// WithTransport sets the transport used for cross-bus communication.
func WithTransport(t Transport) Option {
	return func(b *Bus) { b.transport = t }
}

// WithCodec sets the codec used for serializing event payloads.
func WithCodec(c Codec) Option {
	return func(b *Bus) { b.codec = c }
}

// WithErrorHandler sets the callback for asynchronous errors.
func WithErrorHandler(h ErrorHandler) Option {
	return func(b *Bus) { b.errorHandler = h }
}

// WithAsyncLimit sets the maximum number of concurrent PublishAsync goroutines.
// When the limit is reached, PublishAsync blocks until a slot is freed.
// Values less than 1 are clamped to 1. Default is 4096.
func WithAsyncLimit(n int) Option {
	return func(b *Bus) {
		if n < 1 {
			n = 1
		}
		b.asyncSem = make(chan struct{}, n)
	}
}
