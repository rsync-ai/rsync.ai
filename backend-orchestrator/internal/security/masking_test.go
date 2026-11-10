package security

import "testing"

// TestSensitiveKeysCoverCredentialsAndPII guards the parity fix: the orchestrator
// must redact the same PII keys api-gateway does, in addition to credentials.
func TestSensitiveKeysCoverCredentialsAndPII(t *testing.T) {
	mustMask := []string{
		// credentials
		"password", "api_key", "token", "client_secret", "db_password",
		// PII (parity with api-gateway/internal/security/masking.go)
		"email", "phone", "phone_number", "address", "ssn",
		"social_security", "credit_card", "user_id", "customer_id",
		// case-insensitive + substring behavior
		"Email", "user_email", "billing_address", "Customer_ID",
	}
	for _, k := range mustMask {
		if !IsSensitiveKey(k) {
			t.Errorf("expected key %q to be treated as sensitive", k)
		}
	}
}

// TestEnforcedKeysUnaffectedByPIIAddition pins the safety argument for the only
// live caller (mcp/client.go enforcedKeys): none of the non-sensitive enforced
// keys may start matching just because PII substrings were added.
func TestEnforcedKeysUnaffectedByPIIAddition(t *testing.T) {
	// These enforced keys must NOT be redacted; adding PII keys must not change
	// that (none contains "ssn", "address", "email", "user_id", ...). client_id
	// in particular must stay visible even though client_secret is sensitive.
	mustNotMask := []string{
		"bucket", "bucket_name", "container", "container_name",
		"database", "schema", "warehouse", "account", "role",
		"ssl", "sslmode", "verify_ssl", "host", "hostname", "port",
		"user", "username", "client_id",
	}
	for _, k := range mustNotMask {
		if IsSensitiveKey(k) {
			t.Errorf("enforced key %q must not be sensitive (regression from PII keys)", k)
		}
	}
}
