module github.com/rsync-ai/shared/sqlexec

go 1.26.0

// Go 1.26 like api-gateway, the one module that builds this one: go1.26.9
// (2026-10-08) clears GO-2026-6603, 6605, 6610-6613,
// 6617 (net/http), 6607 (crypto/tls) and 6608 (mime/multipart),
// which no Go 1.25 release fixes (BACKLOG F-GO-1.26-TOOLCHAIN-MOVE).
// go1.26.10 (2026-10-22) clears GO-2026-6809 (crypto/x509).
// go1.26.11 (2026-11-10) clears GO-2026-6859 (net/http).
toolchain go1.26.11

require (
	github.com/go-sql-driver/mysql v1.10.1
	github.com/microsoft/go-mssqldb v1.11.2
	github.com/rsync-ai/shared/pgdriver v0.0.0
	github.com/sirupsen/logrus v1.10.2
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.12.0 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/rsync-ai/shared/pgdriver => ../pgdriver
