package awsclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// closeCounter reports whether the wrapper passed the Close on.
type closeCounter struct {
	io.Reader

	closed bool
}

func (c *closeCounter) Close() error {
	c.closed = true

	return nil
}

func TestRateLimitedBodyPassesEverythingThrough(t *testing.T) {
	source := &closeCounter{Reader: strings.NewReader("hello world")}

	body := newRateLimitedBody(context.Background(), source, rate.NewLimiter(rate.Inf, 0))

	read, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(read) != "hello world" {
		t.Errorf("read %q, want the whole body", read)
	}

	if err := body.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if !source.closed {
		t.Error("the transport could not close the body underneath")
	}
}

func TestRateLimitedBodySlowsTheUploadDown(t *testing.T) {
	// 200 bytes at 1000 B/s with a burst of 100 cannot be done in no time
	source := &closeCounter{Reader: strings.NewReader(strings.Repeat("x", 200))}

	body := newRateLimitedBody(context.Background(), source, rate.NewLimiter(1000, 100))

	start := time.Now()

	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("reading: %v", err)
	}

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("reading took %v, the limiter did not slow it down", elapsed)
	}
}

// A request body is limited on the way out, not only on the way in.
func TestRoundTripLimitsTheRequestBody(t *testing.T) {
	source := &closeCounter{Reader: strings.NewReader("payload")}

	var sent io.ReadCloser

	wrapper := &transportWrapper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sent = req.Body

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
		limiter: rate.NewLimiter(rate.Inf, 0),
	}

	req, err := http.NewRequest(http.MethodPut, "https://example.com/key", source)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	if _, err := wrapper.RoundTrip(req); err != nil {
		t.Fatalf("round trip: %v", err)
	}

	if _, ok := sent.(*rateLimitedBody); !ok {
		t.Fatalf("the transport was handed a %T, want a limited body", sent)
	}

	// the request the caller handed over is left alone
	if req.Body != io.ReadCloser(source) {
		t.Error("RoundTrip modified the request it was given")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
