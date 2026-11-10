package security

import (
	"strings"
)

// SensitiveKeys is the list of keys that should be masked in logs. Pinned, with
// api-gateway's copy and llm-service's SENSITIVE_KEYS, to
// shared/sensitive_keys_golden.json (masking_golden_test.go): change all together.
var SensitiveKeys = []string{
	"password",
	"secret",
	"secret_key",
	"secret_access_key",
	"api_key",
	"apikey",
	"token",
	"access_key",
	"access_key_id",
	"refresh_token",
	"access_token",
	"bearer",
	"authorization",
	"credentials",
	"credentials_json",
	"private_key",
	"client_secret",
	"aws_secret_access_key",
	"aws_access_key_id",
	"connection_string",
	"conn_str",
	"db_password",
	// PII fields — parity with api-gateway/internal/security/masking.go.
	// Matching is case-insensitive SUBSTRING (see isSensitiveKey), so these can
	// over-match composite keys (e.g. "address" ⊃ "ip_address"/"mac_address",
	// "ssn" as a trigram, "user_id" ⊃ "pipeline_user_id"). The sole caller
	// (mcp/client.go enforcedKeys) contains none of these substrings, so its
	// IsSensitiveKey results are unchanged.
	"email",
	"phone",
	"phone_number",
	"address",
	"ssn",
	"social_security",
	"credit_card",
	"user_id", // when used as a query predicate
	"customer_id",
}

// isSensitiveKey checks if a key should be masked (internal use)
func isSensitiveKey(key string) bool {
	keyLower := strings.ToLower(key)
	for _, sensitive := range SensitiveKeys {
		if keyLower == sensitive || strings.Contains(keyLower, sensitive) {
			return true
		}
	}
	return false
}

// IsSensitiveKey checks if a key should be masked (exported for use in other packages)
// Use this to determine if a value should be redacted before logging
func IsSensitiveKey(key string) bool {
	return isSensitiveKey(key)
}
