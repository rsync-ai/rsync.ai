module github.com/rsync-ai/connector-deployer

go 1.26.0

// Pin the patched toolchain so CI/local builds + govulncheck analyze against the
// fixed stdlib. go1.26.9 (2026-10-08) clears GO-2026-6603, 6605, 6610-6613,
// 6617 (net/http), 6607 (crypto/tls) and 6608 (mime/multipart) reachable here,
// and no Go 1.25 release carries those fixes, so this module moved to Go 1.26
// with the services (BACKLOG F-GO-1.26-TOOLCHAIN-MOVE). The `go 1.26.0` line
// makes a 1.25 builder refuse the module; the golang:1.26 Docker base floats to
// this patch. Docker-daemon CVEs stay in .govulncheck-allow.txt (fix is N/A there).
// go1.26.10 (2026-10-22) clears GO-2026-6809 (crypto/x509).
// go1.26.11 (2026-11-10) clears GO-2026-6859 (net/http).
toolchain go1.26.11

require (
	github.com/docker/docker v28.5.2+incompatible
	github.com/docker/go-connections v0.6.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/sys/atomicwriter v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/morikuni/aec v1.1.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)
