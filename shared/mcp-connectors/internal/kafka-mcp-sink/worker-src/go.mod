module github.com/rsync-ai/kafka-mcp-sink-worker

go 1.26.0

// Go 1.26, with the services (BACKLOG F-GO-1.26-TOOLCHAIN-MOVE): go1.26.9
// (2026-10-08) clears the reachable stdlib advisories GO-2026-6603, 6605,
// 6610-6613, 6617 (net/http), 6607 (crypto/tls) and 6608 (net/textproto),
// and no Go 1.25 release carries those fixes. The `go 1.26.0` line makes a
// 1.25 builder refuse the module instead of shipping the unpatched stdlib
// (the Dockerfile's golang:1.26-alpine runs with GOTOOLCHAIN=local).
// go1.26.10 (2026-10-22) clears GO-2026-6809 (crypto/x509).
toolchain go1.26.10

require (
	github.com/rsync-ai/shared/kafkaclient v0.0.0
	github.com/rsync-ai/shared/transforms v0.0.0
	github.com/segmentio/kafka-go v0.4.51
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.12.0 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	golang.org/x/sync v0.24.0 // indirect
)

require (
	github.com/aws/aws-msk-iam-sasl-signer-go v1.0.4 // indirect
	github.com/aws/aws-sdk-go-v2 v1.32.4 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.28.2 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.17.43 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.19 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.12.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.12.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.24.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.28.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.32.4 // indirect
	github.com/aws/smithy-go v1.22.0 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/rsync-ai/shared/memlimit v0.0.0
	github.com/rsync-ai/shared/pgdriver v0.0.0
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/rsync-ai/shared/transforms => ../../../../go/transforms

replace github.com/rsync-ai/shared/kafkaclient => ../../../../go/kafkaclient

replace github.com/rsync-ai/shared/pgdriver => ../../../../go/pgdriver

replace github.com/rsync-ai/shared/memlimit => ../../../../go/memlimit
