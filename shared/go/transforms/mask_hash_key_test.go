package transforms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
)

// KI-PII-HASH-KEY-UNSET: "HMAC-SHA256 (keyed)" hashed with an empty key when no
// key was configured, which anyone can recompute, so a column with a handful of
// values (gender, country) was reversed by hashing each candidate. A keyed hash
// with no key must refuse to run instead of writing a digest that only looks
// keyed.

// setPIIHashKeyForTest points the memoized key at key for one test. The key is
// read once per process, so t.Setenv alone only works for whichever test runs
// first.
func setPIIHashKeyForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv("RSYNC_PII_HASH_SALT", key)
	t.Setenv("PII_HASH_SALT", "")
	_piiHashSaltOnce = sync.Once{}
	t.Cleanup(func() { _piiHashSaltOnce = sync.Once{} })
}

func hmacHex(key, v string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(v))
	return "hmac256:" + hex.EncodeToString(m.Sum(nil))
}

func TestMaskHMAC_WithoutAKey_RefusesToRun(t *testing.T) {
	setPIIHashKeyForTest(t, "")
	for _, hf := range []string{"hmac_sha256", "HMAC-SHA256", "hmac256"} {
		out, _, err := NewSimpleTransformEngine().ApplyMaskWithStats(context.Background(),
			[]Row{{"id": 1, "gender": "female"}},
			map[string]interface{}{"column": "gender", "mask_type": "hash", "hash_function": hf})
		if err == nil {
			t.Fatalf("%s with no key wrote %v; an empty-key HMAC is recomputable by anyone", hf, out)
		}
		if !strings.Contains(err.Error(), "RSYNC_PII_HASH_SALT") {
			t.Errorf("%s: the error does not name the setting to fix: %v", hf, err)
		}
	}
	if PIIHashKeyConfigured() {
		t.Errorf("PIIHashKeyConfigured() = true with no key set")
	}
}

func TestMaskHMAC_WithAKey_IsKeyedByIt(t *testing.T) {
	setPIIHashKeyForTest(t, "install-key-1")
	out, _, err := NewSimpleTransformEngine().ApplyMaskWithStats(context.Background(),
		[]Row{{"id": 1, "gender": "female"}},
		map[string]interface{}{"column": "gender", "mask_type": "hash", "hash_function": "hmac_sha256"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := out[0]["gender"]
	if got != hmacHex("install-key-1", "female") {
		t.Errorf("digest %v is not HMAC-SHA256 under the configured key", got)
	}
	if got == hmacHex("", "female") {
		t.Errorf("digest equals the empty-key HMAC")
	}
	if !PIIHashKeyConfigured() {
		t.Errorf("PIIHashKeyConfigured() = false with a key set")
	}
}

// The control: sha256 and md5 rules saved before a key existed keep running.
// Failing them would stop every pipeline that hashes a column on an install
// that never set the key.
func TestMaskSHA256_WithoutAKey_StillRuns(t *testing.T) {
	setPIIHashKeyForTest(t, "")
	for _, hf := range []string{"sha256", "md5", ""} {
		out, _, err := NewSimpleTransformEngine().ApplyMaskWithStats(context.Background(),
			[]Row{{"id": 1, "email": "a@example.com"}},
			map[string]interface{}{"column": "email", "mask_type": "hash", "hash_function": hf})
		if err != nil {
			t.Fatalf("hash_function %q failed with no key: %v", hf, err)
		}
		if out[0]["email"] == "a@example.com" {
			t.Fatalf("hash_function %q did not hash", hf)
		}
	}
}

// Only a HASH with an HMAC function needs the key: a redact rule that also
// carries a stale hash_function from the UI must still run.
func TestMaskRedact_WithAStaleHMACFunction_StillRuns(t *testing.T) {
	setPIIHashKeyForTest(t, "")
	out, _, err := NewSimpleTransformEngine().ApplyMaskWithStats(context.Background(),
		[]Row{{"id": 1, "gender": "female"}},
		map[string]interface{}{"column": "gender", "mask_type": "redact", "hash_function": "hmac_sha256"})
	if err != nil || out[0]["gender"] != "***" {
		t.Fatalf("redact with a stale hash_function: out=%v err=%v", out, err)
	}
}

// The gateway validates a rule when it is saved. Refusing there means the user
// learns at save time, not from a pipeline that fails on its first batch.
func TestMaskHMAC_WithoutAKey_IsRefusedAtSave(t *testing.T) {
	raw := []map[string]interface{}{{"type": "mask_pii", "order": 1,
		"config": map[string]interface{}{"column": "gender", "mask_type": "hash", "hash_function": "hmac_sha256"}}}

	setPIIHashKeyForTest(t, "")
	if _, _, err := NormalizeAndValidate(raw, "", NormalizeModeSaveCheck); err == nil ||
		!strings.Contains(err.Error(), "RSYNC_PII_HASH_SALT") {
		t.Fatalf("save of an HMAC rule with no key: err=%v, want a refusal naming RSYNC_PII_HASH_SALT", err)
	}

	setPIIHashKeyForTest(t, "install-key-1")
	if _, _, err := NormalizeAndValidate(raw, "", NormalizeModeSaveCheck); err != nil {
		t.Fatalf("save of an HMAC rule with a key: %v", err)
	}
}
