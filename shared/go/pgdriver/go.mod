module github.com/rsync-ai/shared/pgdriver

go 1.26.0

// Go 1.26 like the three services that build this module: go1.26.9
// (2026-10-08) clears GO-2026-6607 (crypto/tls),
// which no Go 1.25 release fixes (BACKLOG F-GO-1.26-TOOLCHAIN-MOVE).
// go1.26.10 (2026-10-22) clears GO-2026-6809 (crypto/x509).
toolchain go1.26.10

require github.com/jackc/pgx/v5 v5.10.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
