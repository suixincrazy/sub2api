# Proxy Integration

The proxy implementation is adapted from `smmooooonn/sub2api-xray` through
`84ed37c31`. The main project keeps its existing account, group, subscription,
redemption, export and branding behavior. The Xray user resource workbench is
not included.

## Supported Inputs

- Standard HTTP, HTTPS, SOCKS5 and SOCKS5h proxies.
- VMess, VLESS, Trojan, Shadowsocks, Hysteria, Hysteria2, TUIC, AnyTLS,
  Naive and WireGuard share links.
- Base64 subscriptions, Clash YAML/JSON, and sing-box JSON subscriptions.
- Native sing-box outbounds and WireGuard endpoints, including TLS trust,
  transport, authentication and multiple peer settings.

Administrators can import nodes and manage subscription sources in the proxy
page. Sources support manual and scheduled refresh, traffic/expiry metadata,
and disabling nodes removed by a refresh. Modern node backups retain their
full connection configuration. Disabled or expired proxies fail closed.

HTTP/SOCKS proxies use the existing application transport. Modern nodes run
through a local SOCKS listener managed by Xray or sing-box. Runtime listeners
bind to loopback. Node configuration changes invalidate account caches and
runtime connections; label-only changes and unchanged refreshes preserve them.
WebSocket reuse also checks the upstream address and proxy address.

## Runtime Versions

The Dockerfiles package Xray `26.3.27` and the musl build of sing-box `1.13.14`.
The latter includes Naive, WireGuard, QUIC and uTLS support. Downloaded sing-box
archives are checked against the pinned SHA256 values in each Dockerfile.

Configure `XRAY_BIN` and `SING_BOX_BIN` for installations outside Docker.
`XRAY_WORK_DIR` and `SING_BOX_WORK_DIR` must be writable by the application.
The default runtime idle timeout is zero: a long-lived request must not lose
its proxy process merely because it has not opened another connection.

Xray 26 removed `allowInsecure`. For legacy nodes that request it, the runtime
manager obtains the peer certificate and pins its SHA256 for the connection.
Clash ALPN and certificate verification options survive import. Native
sing-box TLS settings are preserved without conversion through a share link.

## Validation and Deployment

The application version follows `Wei-Shaw/sub2api` through
`backend/cmd/server/VERSION`. Keep fork-specific build identity in the Git
commit and image revision label. Local builds should use the embedded version
instead of adding a proxy or date suffix through `main.Version`. Update
artifacts remain builds of this fork so the proxy features are retained.

Relevant backend regression tests:

```sh
cd backend
go test -tags unit ./internal/service ./internal/handler/admin \
  ./internal/repository ./internal/pkg/tlsfingerprint ./internal/pkg/proxyutil \
  -run '(Proxy|Xray|SingBox|Singbox|Dialer|OpenAIWS)' -timeout 180s
```

Build the frontend before compiling the embedded server:

```sh
cd frontend
pnpm install --frozen-lockfile
pnpm run build
cd ../backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath \
  -ldflags="-s -w -X main.Commit=$(git rev-parse HEAD)" -o sub2api ./cmd/server
```

Migrations `239_add_xray_proxy_support.sql` and
`241_complete_proxy_source_schema.sql` support clean installations and the
legacy `proxy_sources.url` schema. Back up the database and application
environment before upgrading an existing installation. Preserve the actual
database and application mounts. Replace only the application service when
the database and Redis do not need an upgrade.

Configuration parsing, runtime configuration checks, and actual provider
requests are separate checks. A successful import or account configuration
does not establish upstream connectivity.
