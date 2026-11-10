-- 125_assistant_model_keys.sql
-- The model keys the in-app assistant answers with, set by a platform admin in
-- Admin -> Settings -> AI models (docs/internal/rsync-assistant-design.md).
--
-- One row per entry the picker offers. The key is shared/go/crypto ciphertext,
-- the same as the SMTP password, and is never returned by any route: the panel
-- shows only that a key is set. api-gateway decrypts it per model step and
-- sends it to llm-service with the internal secret; llm-service never holds
-- ENCRYPTION_KEY. A row overrides the environment key for its entry; deleting
-- it falls back to the environment (ANTHROPIC_API_KEY, OPENAI_API_KEY,
-- GEMINI_API_KEY). model is a model id, never a URL (checked before it is
-- stored); NULL means the default. The admin key rotation re-encrypts
-- key_encrypted (admin_crypto_rotation.go).

CREATE TABLE IF NOT EXISTS assistant_model_keys (
    entry          TEXT PRIMARY KEY CHECK (entry IN ('claude', 'chatgpt', 'gemini')),
    key_encrypted  TEXT NOT NULL CHECK (key_encrypted <> ''),
    model          TEXT,
    updated_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE assistant_model_keys IS
'Instance-wide model keys for the in-app assistant, one row per picker entry, set by a platform admin. key_encrypted is shared/go/crypto ciphertext and never returned. A row overrides the environment key for its entry; no row falls back to it.';
