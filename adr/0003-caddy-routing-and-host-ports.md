# 0003. Caddy routing and host ports

Status: Accepted.

## Decision

The launch API generates a stack ID with an adverb, a noun, and the UTC date,
such as `quietly-otter-20261121`. It can add a numeric suffix if candidate IDs
collide. The hostname is `<stack-id>.<base_domain>` and stays the same for the
stack's lifetime. The operator sets `base_domain` and supplies wildcard DNS
pointing to the Caddy ingress and a matching wildcard certificate. Host
reachability is configured separately.

One Caddy instance shares ports 80 and 443 across all stacks. For the single
HTTP endpoint declared in [ADR 0002](0002-compose-and-image-sources.md),
Podman assigns a host port bound to `127.0.0.1`. Caddy targets that port
over HTTP. The launcher publishes the route after the runtime checks and any
fresh-launch commands. It records the stack as ready after publication and
withdraws the route before releasing the port.

The optional `x-launcher.endpoint.https` field is a boolean that defaults
to `true`.

```yaml
x-launcher:
  endpoint:
    service: web
    port: 8080
    https: true
```

| Setting | Port 80 | Port 443 |
| --- | --- | --- |
| `https: true` or omitted | Caddy returns an HTTPS redirect for the same hostname, path, and query. Plain HTTP never reaches the app. | Caddy terminates TLS and proxies the request over HTTP to the app's loopback port. |
| `https: false` | Caddy matches plain HTTP requests and proxies them to the app. | The shared TLS listener returns 404 for this hostname. |

The launcher updates one hostname route through Caddy's local Unix admin
socket. The route matches HTTP and HTTPS separately, and a single config update
applies both behaviors. See the [Caddy setup](../app/README.md#caddy-routes) for
the supported ingress configuration.

Additional TCP ports are opt-in through Compose `ports` and bind only on
host loopback. The launcher reports the actual addresses for direct use
after SSH. These ports are outside Caddy routing and can change when
containers are recreated.

[Caddy protocol matcher](https://caddyserver.com/docs/caddyfile/matchers#protocol) ·
[Caddy redirects](https://caddyserver.com/docs/caddyfile/directives/redir) ·
[Caddy wildcard certificates](https://caddyserver.com/docs/automatic-https#wildcard-certificates)
