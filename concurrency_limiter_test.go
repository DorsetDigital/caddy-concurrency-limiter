package concurrencylimiter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (f handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	return f(w, r)
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		handler Handler
		wantErr bool
	}{
		{"valid", Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable}, false},
		{"zero max", Handler{MaxConcurrent: 0, StatusCode: http.StatusServiceUnavailable}, true},
		{"negative max", Handler{MaxConcurrent: -1, StatusCode: http.StatusServiceUnavailable}, true},
		{"invalid low status", Handler{MaxConcurrent: 1, StatusCode: 399}, true},
		{"invalid high status", Handler{MaxConcurrent: 1, StatusCode: 600}, true},
		{"negative retry", Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable, RetryAfter: -1}, true},
		{"retry with unsupported status", Handler{MaxConcurrent: 1, StatusCode: http.StatusBadGateway, RetryAfter: 1}, true},
		{"retry with 429", Handler{MaxConcurrent: 1, StatusCode: http.StatusTooManyRequests, RetryAfter: 1}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.handler.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConcurrencyLimitRejectsExcessRequest(t *testing.T) {
	h := &Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable, RetryAfter: 2}

	entered := make(chan struct{})
	release := make(chan struct{})

	next := handlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	firstDone := make(chan error, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		firstDone <- h.ServeHTTP(rec, req, next)
	}()

	<-entered

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	err := h.ServeHTTP(rec, req, next)

	var handlerErr caddyhttp.HandlerError
	if !errorAs(err, &handlerErr) {
		t.Fatalf("expected caddyhttp.HandlerError, got %T (%v)", err, err)
	}
	if handlerErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", handlerErr.StatusCode, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want %q", got, "2")
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first request returned error: %v", err)
	}
}

func TestCounterReleasedAfterDownstreamError(t *testing.T) {
	h := &Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable}

	var calls atomic.Int64
	next := handlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		calls.Add(1)
		return caddyhttp.Error(http.StatusBadGateway, nil)
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err := h.ServeHTTP(httptest.NewRecorder(), req, next); err == nil {
		t.Fatal("expected downstream error")
	}

	req = httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err := h.ServeHTTP(httptest.NewRecorder(), req, next); err == nil {
		t.Fatal("expected downstream error")
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("downstream calls = %d, want 2", got)
	}
}

func TestNeverExceedsConfiguredConcurrency(t *testing.T) {
	const limit = int64(4)
	const requests = 40

	h := &Handler{MaxConcurrent: limit, StatusCode: http.StatusServiceUnavailable}

	var current atomic.Int64
	var peak atomic.Int64

	next := handlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		now := current.Add(1)
		defer current.Add(-1)

		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(requests)

	for range requests {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			_ = h.ServeHTTP(httptest.NewRecorder(), req, next)
		}()
	}

	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, limit = %d", got, limit)
	}
}

func TestCounterReleasedAfterPanic(t *testing.T) {
	h := &Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected downstream panic")
			}
		}()

		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		_ = h.ServeHTTP(httptest.NewRecorder(), req, handlerFunc(func(http.ResponseWriter, *http.Request) error {
			panic("boom")
		}))
	}()

	if got := h.active.Load(); got != 0 {
		t.Fatalf("active after panic = %d, want 0", got)
	}
}

func TestCounterReleasedAfterCancellation(t *testing.T) {
	h := &Handler{MaxConcurrent: 1, StatusCode: http.StatusServiceUnavailable}

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)

	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil).WithContext(ctx)
	go func() {
		done <- h.ServeHTTP(httptest.NewRecorder(), req, handlerFunc(func(http.ResponseWriter, *http.Request) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}))
	}()

	<-entered
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("downstream error = %v, want context.Canceled", err)
	}
	if got := h.active.Load(); got != 0 {
		t.Fatalf("active after cancellation = %d, want 0", got)
	}
}

func TestAcquireDoesNotOvershootLimit(t *testing.T) {
	const limit = int64(8)
	const attempts = 100

	h := &Handler{MaxConcurrent: limit}

	var admitted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(attempts)

	start := make(chan struct{})
	for range attempts {
		go func() {
			defer wg.Done()
			<-start
			if acquired, _ := h.acquire(); acquired {
				admitted.Add(1)
			}
		}()
	}

	close(start)
	wg.Wait()

	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted = %d, want %d", got, limit)
	}
	if got := h.active.Load(); got != limit {
		t.Fatalf("active = %d, want %d", got, limit)
	}
}

func TestCaddyfileUnmarshal(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantMax    int64
		wantStatus int
		wantRetry  int
		wantErr    bool
	}{
		{"short form", "concurrency_limit 10", 10, 0, 0, false},
		{"block form", "concurrency_limit {\n max 12\n status_code 429\n retry_after 3\n}", 12, 429, 3, false},
		{"missing max", "concurrency_limit {\n status_code 503\n}", 0, 0, 0, true},
		{"invalid max", "concurrency_limit nope", 0, 0, 0, true},
		{"extra argument", "concurrency_limit 10 extra", 0, 0, 0, true},
		{"duplicate max", "concurrency_limit 10 {\n max 20\n}", 0, 0, 0, true},
		{"unknown option", "concurrency_limit {\n nope 1\n}", 0, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Handler
			d := caddyfile.NewTestDispenser(tt.input)
			err := h.UnmarshalCaddyfile(d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnmarshalCaddyfile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if h.MaxConcurrent != tt.wantMax || h.StatusCode != tt.wantStatus || h.RetryAfter != tt.wantRetry {
				t.Fatalf("got max=%d status=%d retry=%d; want max=%d status=%d retry=%d",
					h.MaxConcurrent, h.StatusCode, h.RetryAfter, tt.wantMax, tt.wantStatus, tt.wantRetry)
			}
		})
	}
}

func errorAs(err error, target any) bool {
	return errors.As(err, target)
}
