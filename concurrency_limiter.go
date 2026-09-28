package concurrencylimiter

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("concurrency_limit", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("concurrency_limit", httpcaddyfile.Before, "reverse_proxy")
}

// Handler limits the number of requests concurrently executing the remainder
// of the handler chain.
//
// Each configured handler instance owns its own counter. This makes the module
// suitable for per-site, per-route, or per-origin protection without coupling
// it to any specific upstream implementation.
type Handler struct {
	// Maximum number of requests allowed to execute concurrently.
	MaxConcurrent int64 `json:"max_concurrent"`

	// HTTP status code returned when the concurrency limit has been reached.
	// Defaults to 503 Service Unavailable.
	StatusCode int `json:"status_code,omitempty"`

	// Optional Retry-After header value, in seconds. A value of 0 omits the
	// header. Defaults to 0.
	RetryAfter int `json:"retry_after,omitempty"`

	logger *zap.Logger
	active atomic.Int64
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.concurrency_limit",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision applies defaults.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)
	if h.StatusCode == 0 {
		h.StatusCode = http.StatusServiceUnavailable
	}
	return nil
}

// Validate validates the handler configuration.
func (h *Handler) Validate() error {
	if h.MaxConcurrent <= 0 {
		return errors.New("max_concurrent must be greater than zero")
	}

	if h.StatusCode < 400 || h.StatusCode > 599 {
		return fmt.Errorf("status_code must be between 400 and 599, got %d", h.StatusCode)
	}

	if h.RetryAfter < 0 {
		return errors.New("retry_after must be zero or greater")
	}

	if h.RetryAfter > 0 && h.StatusCode != http.StatusServiceUnavailable && h.StatusCode != http.StatusTooManyRequests {
		return errors.New("retry_after is only supported with status_code 429 or 503")
	}

	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	acquired, active := h.acquire()
	if !acquired {
		if h.logger != nil {
			h.logger.Warn("concurrency limit exceeded",
				zap.String("host", r.Host),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int64("limit", h.MaxConcurrent),
				zap.Int64("active", active),
			)
		}

		if h.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(h.RetryAfter))
		}

		return caddyhttp.Error(h.StatusCode, nil)
	}

	defer h.active.Add(-1)

	return next.ServeHTTP(w, r)
}

// acquire reserves one concurrency slot without allowing the counter to
// transiently exceed the configured maximum.
func (h *Handler) acquire() (bool, int64) {
	for {
		current := h.active.Load()
		if current >= h.MaxConcurrent {
			return false, current
		}
		if h.active.CompareAndSwap(current, current+1) {
			return true, current + 1
		}
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
// Syntax:
//
//	concurrency_limit <max>
//	concurrency_limit {
//		max <max>
//		status_code <code>
//		retry_after <seconds>
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()

	maxSet := false
	if d.NextArg() {
		max, err := strconv.ParseInt(d.Val(), 10, 64)
		if err != nil {
			return d.Errf("invalid concurrency limit %q: %v", d.Val(), err)
		}
		h.MaxConcurrent = max
		maxSet = true

		if d.NextArg() {
			return d.ArgErr()
		}
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "max":
			if maxSet {
				return d.Err("concurrency limit max may only be specified once")
			}
			if !d.NextArg() {
				return d.ArgErr()
			}
			max, err := strconv.ParseInt(d.Val(), 10, 64)
			if err != nil {
				return d.Errf("invalid max value %q: %v", d.Val(), err)
			}
			h.MaxConcurrent = max
			maxSet = true
			if d.NextArg() {
				return d.ArgErr()
			}

		case "status_code":
			if !d.NextArg() {
				return d.ArgErr()
			}
			code, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid status code %q: %v", d.Val(), err)
			}
			h.StatusCode = code
			if d.NextArg() {
				return d.ArgErr()
			}

		case "retry_after":
			if !d.NextArg() {
				return d.ArgErr()
			}
			seconds, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid retry_after value %q: %v", d.Val(), err)
			}
			h.RetryAfter = seconds
			if d.NextArg() {
				return d.ArgErr()
			}

		default:
			return d.Errf("unrecognized subdirective %q", d.Val())
		}
	}

	if h.MaxConcurrent == 0 {
		return d.Err("a concurrency limit must be specified")
	}

	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var limiter Handler
	if err := limiter.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &limiter, nil
}

var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
