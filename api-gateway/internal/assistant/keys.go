package assistant

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rsync-ai/shared/crypto"
)

// The model keys a platform admin sets in Admin > Settings > AI models
// (migration 125). A key is shared/go/crypto ciphertext at rest, decrypted
// here once per question and sent to llm-service with the internal secret;
// llm-service never holds ENCRYPTION_KEY. No route returns a key: the list
// below never reads the column.

// KeyRow is what the admin panel shows for a saved key. It has no key field.
type KeyRow struct {
	Entry     string
	Model     string
	UpdatedBy string // the admin's email, "" once that user is deleted
	UpdatedAt time.Time
}

// Keys reads and writes the panel keys.
type Keys interface {
	// Get returns the decrypted key and model for entry; ok is false when the
	// panel has no key for it, so llm-service falls back to its environment.
	Get(ctx context.Context, entry string) (key, model string, ok bool, err error)
	List(ctx context.Context) ([]KeyRow, error)
	// SetKey saves (or replaces) entry's key and model.
	SetKey(ctx context.Context, entry, key, model, adminID string) error
	// SetModel changes the model of a saved key; false when there is none.
	SetModel(ctx context.Context, entry, model, adminID string) (bool, error)
	Remove(ctx context.Context, entry string) (bool, error)
}

// ErrUndecryptable is a saved key the current keyring cannot decrypt.
var ErrUndecryptable = errors.New("the saved key cannot be decrypted with this instance's ENCRYPTION_KEY; an admin should set it again")

// DBKeys is Keys on the assistant_model_keys table.
type DBKeys struct {
	DB func() *sql.DB
}

func (k DBKeys) db() (*sql.DB, error) {
	if k.DB == nil || k.DB() == nil {
		return nil, errors.New("database not connected")
	}
	return k.DB(), nil
}

func (k DBKeys) Get(ctx context.Context, entry string) (string, string, bool, error) {
	database, err := k.db()
	if err != nil {
		return "", "", false, err
	}
	var cipher, model string
	err = database.QueryRowContext(ctx,
		`SELECT key_encrypted, COALESCE(model, '') FROM assistant_model_keys WHERE entry = $1`, entry,
	).Scan(&cipher, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	key, err := crypto.DecryptString(cipher)
	if err != nil || key == "" {
		return "", "", false, ErrUndecryptable
	}
	return key, model, true, nil
}

func (k DBKeys) List(ctx context.Context) ([]KeyRow, error) {
	database, err := k.db()
	if err != nil {
		return nil, err
	}
	rows, err := database.QueryContext(ctx, `
		SELECT k.entry, COALESCE(k.model, ''), COALESCE(u.email, ''), k.updated_at
		FROM assistant_model_keys k LEFT JOIN users u ON u.id = k.updated_by
		ORDER BY k.entry`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRow
	for rows.Next() {
		var r KeyRow
		if err := rows.Scan(&r.Entry, &r.Model, &r.UpdatedBy, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (k DBKeys) SetKey(ctx context.Context, entry, key, model, adminID string) error {
	database, err := k.db()
	if err != nil {
		return err
	}
	cipher, err := crypto.EncryptString(key)
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO assistant_model_keys (entry, key_encrypted, model, updated_by, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')::uuid, NOW())
		ON CONFLICT (entry) DO UPDATE SET key_encrypted = EXCLUDED.key_encrypted,
			model = EXCLUDED.model, updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		entry, cipher, model, adminID)
	return err
}

func (k DBKeys) SetModel(ctx context.Context, entry, model, adminID string) (bool, error) {
	database, err := k.db()
	if err != nil {
		return false, err
	}
	res, err := database.ExecContext(ctx, `
		UPDATE assistant_model_keys SET model = NULLIF($2, ''), updated_by = NULLIF($3, '')::uuid, updated_at = NOW()
		WHERE entry = $1`, entry, model, adminID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (k DBKeys) Remove(ctx context.Context, entry string) (bool, error) {
	database, err := k.db()
	if err != nil {
		return false, err
	}
	res, err := database.ExecContext(ctx, `DELETE FROM assistant_model_keys WHERE entry = $1`, entry)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
