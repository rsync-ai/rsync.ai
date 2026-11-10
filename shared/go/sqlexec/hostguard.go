package sqlexec

import (
	"fmt"
	"net"
	"strings"
)

// isLocalDBHost reports whether host points at a local/dev or docker-internal
// database where TLS is typically not configured. Empty and explicit loopback
// names are local. IP LITERALS are classified by address (loopback/RFC1918/ULA/
// link-local/CGNAT → local), so an IPv6 literal or a public IPv4 literal is
// correctly treated remote instead of by a textual "." heuristic that missed
// them. Non-literal hostnames keep the dotless=local heuristic (docker service
// names). NOTE residual: a dotless single-label hostname that resolves to a
// PUBLIC address is still treated local — closing that needs address resolution
// with an injectable resolver, tracked as a follow-up; the address-based literal
// check plus verify-by-default for dotted remotes covers the realistic surface.
func isLocalDBHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.Trim(h, "[]") // tolerate bracketed IPv6 literals
	if h == "" {
		return true
	}
	switch h {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return isPrivateOrLoopbackIP(ip)
	}
	// Non-literal hostname: dotless single-label names are docker-internal/local
	// DNS; any dotted hostname is remote so callers default to verified TLS.
	// A Kubernetes in-cluster name (*.svc, *.cluster.local) is local too: neither
	// suffix resolves on public DNS (shared/local_db_host_golden.json).
	return !strings.Contains(h, ".") || strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".cluster.local")
}

// isPrivateOrLoopbackIP reports whether ip is a loopback, RFC1918/ULA private,
// link-local, or CGNAT (100.64.0.0/10) address — i.e. NOT a public destination.
func isPrivateOrLoopbackIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true // 100.64.0.0/10 carrier-grade NAT
	}
	return false
}

// resolvePostgresSSLMode picks the pgx/libpq sslmode for an outbound PostgreSQL
// connection. An explicit config value wins (ssl_mode or sslmode key, nil-safe
// and trimmed). Otherwise the default is host-based: local/docker-internal hosts
// get "disable" (dev/e2e Postgres rarely runs TLS), while any remote host — e.g.
// Azure Managed PostgreSQL — defaults to "verify-full": encrypt AND verify the
// server certificate + hostname against the image trust store (Dockerfile ships
// ca-certificates covering the standard cloud roots). Verifying by default closes
// the MITM window the previous "require" (encrypt-without-verify) default left
// open on customer-DB traffic. A server whose CA is not in the trust store
// (self-managed / some RDS / GCP) opts out by setting ssl_mode=require on the
// connection — which still encrypts, just without CA/hostname verification.
func resolvePostgresSSLMode(config map[string]interface{}, host string) string {
	for _, key := range []string{"ssl_mode", "sslmode"} {
		if v, ok := config[key]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	if isLocalDBHost(host) {
		return "disable"
	}
	return "verify-full"
}

// resolveMySQLTLSMode picks the go-sql-driver/mysql DSN "tls" parameter for an
// outbound MySQL connection. It mirrors resolvePostgresSSLMode: an explicit
// config value wins (ssl_mode, sslmode, tls, or tls_mode key, nil-safe and
// trimmed) and is normalised to a value the driver accepts; otherwise the
// default is host-based. Local/docker-internal hosts get "false" (dev/e2e
// MySQL rarely runs TLS), while any remote host — e.g. Azure Database for
// MySQL — defaults to "true": encrypt AND verify the server certificate against
// the image trust store (the Dockerfile ships ca-certificates, covering the
// standard cloud roots incl. Azure's DigiCert/Microsoft chain). Verifying by
// default closes the MITM window the previous "skip-verify" default left open
// on customer-DB traffic. If a connection targets a MySQL whose CA is not in
// the trust store (some self-managed / RDS / GCP certs), the user opts out
// explicitly by setting ssl_mode=require (or skip-verify) on the connection —
// which still encrypts, just without CA/hostname verification.
func resolveMySQLTLSMode(config map[string]interface{}, host string) string {
	for _, key := range []string{"ssl_mode", "sslmode", "tls", "tls_mode"} {
		if v, ok := config[key]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return normalizeMySQLTLS(s)
			}
		}
	}
	if isLocalDBHost(host) {
		return "false"
	}
	return "true"
}

// normalizeMySQLTLS maps assorted SSL/TLS spellings — libpq-style ("require",
// "disable", "verify-full"), booleans, and native go-sql-driver values — onto
// the four values the mysql driver understands: "false", "true",
// "skip-verify", "preferred". Unrecognised values pass through unchanged so a
// custom config name registered via mysql.RegisterTLSConfig still works.
func normalizeMySQLTLS(s string) string {
	switch strings.ToLower(s) {
	case "disable", "disabled", "false", "off", "0", "none":
		return "false"
	case "require", "required", "skip-verify", "skip_verify":
		return "skip-verify"
	case "prefer", "preferred":
		return "preferred"
	case "verify-ca", "verify_ca", "verify-full", "verify_full", "verify-identity", "verify_identity", "true", "on", "1":
		return "true"
	default:
		return s
	}
}
