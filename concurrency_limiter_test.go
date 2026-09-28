package concurrencylimiter

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// errorAs is kept local so the test file does not need to obscure the test
// intent with repetitive boilerplate.
func errorAs(err error, target any) bool {
	switch t := target.(type) {
	case *caddyhttp.HandlerError:
		handlerErr, ok := err.(caddyhttp.HandlerError)
		if ok {
			*t = handlerErr
		}
		return ok
	default:
		panic("unsupported target type")
	}
}
