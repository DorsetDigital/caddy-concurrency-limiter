# Caddy Concurrency Limiter

A generic Caddy HTTP middleware module that limits the number of requests which
may concurrently execute the remainder of a handler chain.

It is intended for noisy-neighbour protection, origin protection, and other
cases where request *concurrency* is a better signal of resource pressure than
requests per second.

The module is not tied to PHP, FastCGI, `reverse_proxy`, or any particular
upstream. Place it before whichever handlers you want to protect.

## Status

Initial development. Do not treat this repository as production-ready until a
stable release has been published.

## Caddyfile

Simple form:

```caddyfile
example.com {
	concurrency_limit 10
	reverse_proxy 10.0.0.10:8080
}
```

Expanded form:

```caddyfile
example.com {
	concurrency_limit {
		max 10
		status_code 503
		retry_after 2
	}

	reverse_proxy 10.0.0.10:8080
}
```

The handler can also protect a route whose final handler is not a reverse
proxy:

```caddyfile
example.com {
	route /expensive/* {
		concurrency_limit 5
		# Any following Caddy handlers may be placed here.
	}
}
```

### Behaviour

Each configured `concurrency_limit` handler instance maintains its own local
counter. When `max` requests are already active, additional requests are
rejected immediately.

The default rejection status is `503 Service Unavailable`. `retry_after`
may optionally add a `Retry-After` response header expressed in seconds.

Counters are intentionally process-local. In a multi-node Caddy deployment,
the configured limit applies independently on each Caddy instance. This keeps
the request path free of distributed locks or external datastore dependencies.

A request occupies a slot until the next handler returns. For a reverse proxy,
that normally means until the proxied request has completed. Long-lived
requests such as streaming responses therefore hold a slot for their lifetime.

## JSON

The native Caddy JSON module ID is:

```text
http.handlers.concurrency_limit
```

Example:

```json
{
	"handler": "concurrency_limit",
	"max_concurrent": 10,
	"status_code": 503,
	"retry_after": 2
}
```

## Building Caddy

Using `xcaddy`:

```sh
xcaddy build \
	--with github.com/DorsetDigital/caddy-concurrency-limiter
```

## Design principles

- Generic HTTP middleware; no coupling to PHP or a particular origin type.
- Fail fast when capacity is exhausted; no unbounded request queue.
- Handler-local counters for predictable isolation between independently
  configured sites/routes.
- No distributed lock or datastore in the request path.
- Counters are released with `defer`, including when downstream handlers
  return an error.
- Invalid limits and response codes are rejected at configuration validation.

## Security notes

This module is intended as one layer of resource isolation, not as a substitute
for upstream resource limits, request timeouts, authentication, WAF controls,
or application-level capacity planning.

Because long-lived responses consume slots for their full lifetime, apply
limits deliberately to WebSocket, SSE, streaming, and similar routes.

## License

MIT
