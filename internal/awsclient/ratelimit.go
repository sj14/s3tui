package awsclient

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// rateLimitedReader limits how fast the wrapped reader is drained.
type rateLimitedReader struct {
	ctx     context.Context
	reader  io.Reader
	limiter *rate.Limiter
}

func newRateLimitedReader(ctx context.Context, reader io.Reader, limiter *rate.Limiter) io.Reader {
	return &rateLimitedReader{ctx: ctx, reader: reader, limiter: limiter}
}

// newRateLimitedBody limits a request body, which the transport also has to be
// able to close.
func newRateLimitedBody(ctx context.Context, body io.ReadCloser, limiter *rate.Limiter) io.ReadCloser {
	return &rateLimitedBody{
		Reader: newRateLimitedReader(ctx, body, limiter),
		closer: body,
	}
}

type rateLimitedBody struct {
	io.Reader

	closer io.Closer
}

func (b *rateLimitedBody) Close() error { return b.closer.Close() }

func (r *rateLimitedReader) Read(buf []byte) (int, error) {
	if burst := r.limiter.Burst(); burst > 0 && len(buf) > burst {
		buf = buf[:burst]
	}

	n, err := r.reader.Read(buf)
	if n <= 0 {
		return n, err
	}

	if waitErr := r.limiter.WaitN(r.ctx, n); waitErr != nil {
		return n, waitErr
	}

	return n, err
}
