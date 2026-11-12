package assistant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"
)

const testEncryptionKey = "unit-test-encryption-key-0123456789ab"

// recordingMatcher matches by regexp, like sqlmock's default, and keeps every
// statement it saw so a test can check what was never selected.
type recordingMatcher struct {
	mu   sync.Mutex
	seen []string
}

func (m *recordingMatcher) Match(expected, actual string) error {
	m.mu.Lock()
	m.seen = append(m.seen, actual)
	m.mu.Unlock()
	return sqlmock.QueryMatcherRegexp.Match(expected, actual)
}

func newKeysDB(t *testing.T) (DBKeys, sqlmock.Sqlmock, *recordingMatcher) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEYS", "")
	t.Setenv("ENCRYPTION_KEY", testEncryptionKey)
	matcher := &recordingMatcher{}
	database, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return DBKeys{DB: func() *sql.DB { return database }}, mock, matcher
}

// capture is a sqlmock argument that keeps the value it was given.
type capture struct{ value string }

func (c *capture) Match(v driver.Value) bool {
	s, ok := v.(string)
	c.value = s
	return ok
}

func TestSetKeyStoresCiphertextThatDecryptsToTheKey(t *testing.T) {
	keys, mock, _ := newKeysDB(t)
	key := "unit-test-provider-key-canary"
	cipher := &capture{}
	mock.ExpectExec(`INSERT INTO assistant_model_keys`).
		WithArgs("claude", cipher, "claude-opus-5-5", "6f1d2b9e-1c1e-4f43-8d55-0d1f2b6e2a10").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := keys.SetKey(context.Background(), "claude", key, "claude-opus-5-5", "6f1d2b9e-1c1e-4f43-8d55-0d1f2b6e2a10"); err != nil {
		t.Fatal(err)
	}
	if cipher.value == "" || strings.Contains(cipher.value, key) {
		t.Fatalf("the stored value must be ciphertext, not the key (empty=%v)", cipher.value == "")
	}
	plain, err := crypto.DecryptString(cipher.value)
	if err != nil || plain != key {
		t.Fatalf("stored value does not decrypt to the key: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetDecryptsAndFallsBack(t *testing.T) {
	keys, mock, _ := newKeysDB(t)
	cipher, err := crypto.EncryptString("unit-test-gemini-key")
	if err != nil {
		t.Fatal(err)
	}
	q := `SELECT key_encrypted, COALESCE\(model, ''\) FROM assistant_model_keys WHERE entry = \$1`
	mock.ExpectQuery(q).WithArgs("gemini").WillReturnRows(sqlmock.NewRows([]string{"key_encrypted", "model"}).AddRow(cipher, "gemini-2.5-pro"))
	mock.ExpectQuery(q).WithArgs("chatgpt").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(q).WithArgs("claude").WillReturnRows(sqlmock.NewRows([]string{"key_encrypted", "model"}).AddRow("not-ciphertext", ""))

	key, model, ok, err := keys.Get(context.Background(), "gemini")
	if err != nil || !ok || key != "unit-test-gemini-key" || model != "gemini-2.5-pro" {
		t.Fatalf("Get(gemini) = %q %q %v %v", key, model, ok, err)
	}
	if _, _, ok, err := keys.Get(context.Background(), "chatgpt"); ok || err != nil {
		t.Fatalf("no row: ok %v err %v, want the environment fallback", ok, err)
	}
	if _, _, _, err := keys.Get(context.Background(), "claude"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("bad ciphertext: err %v, want ErrUndecryptable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetUnderAnotherKeyringIsUndecryptable(t *testing.T) {
	t.Setenv("ENCRYPTION_KEYS", "")
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-other-instance-01")
	cipher, err := crypto.EncryptString("unit-test-claude-key")
	if err != nil {
		t.Fatal(err)
	}
	keys, mock, _ := newKeysDB(t)
	mock.ExpectQuery(`SELECT key_encrypted`).WithArgs("claude").WillReturnRows(sqlmock.NewRows([]string{"key_encrypted", "model"}).AddRow(cipher, ""))
	if _, _, _, err := keys.Get(context.Background(), "claude"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("err %v, want ErrUndecryptable", err)
	}
}

func TestListNeverReadsTheKey(t *testing.T) {
	keys, mock, matcher := newKeysDB(t)
	at := time.Unix(1700000000, 0).UTC()
	mock.ExpectQuery(`FROM assistant_model_keys k LEFT JOIN users u`).WillReturnRows(
		sqlmock.NewRows([]string{"entry", "model", "email", "updated_at"}).
			AddRow("chatgpt", "", "admin@example.com", at).
			AddRow("claude", "claude-opus-5-5", "", at))
	rows, err := keys.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != (KeyRow{"chatgpt", "", "admin@example.com", at}) || rows[1].Model != "claude-opus-5-5" {
		t.Fatalf("rows = %+v", rows)
	}
	for _, q := range matcher.seen {
		if regexp.MustCompile(`(?i)key_encrypted`).MatchString(q) {
			t.Fatalf("List selects the key column: %s", q)
		}
	}
}

func TestSetModelAndRemoveReportAMissingRow(t *testing.T) {
	keys, mock, _ := newKeysDB(t)
	mock.ExpectExec(`UPDATE assistant_model_keys SET model`).WithArgs("claude", "", "").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM assistant_model_keys WHERE entry = \$1`).WithArgs("claude").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM assistant_model_keys WHERE entry = \$1`).WithArgs("gemini").WillReturnResult(sqlmock.NewResult(0, 1))
	if found, err := keys.SetModel(context.Background(), "claude", "", ""); found || err != nil {
		t.Fatalf("SetModel with no row = %v %v", found, err)
	}
	if removed, err := keys.Remove(context.Background(), "claude"); removed || err != nil {
		t.Fatalf("Remove with no row = %v %v", removed, err)
	}
	if removed, err := keys.Remove(context.Background(), "gemini"); !removed || err != nil {
		t.Fatalf("Remove = %v %v", removed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestKeysWithoutADatabaseFail(t *testing.T) {
	keys := DBKeys{DB: func() *sql.DB { return nil }}
	if _, _, _, err := keys.Get(context.Background(), "claude"); err == nil {
		t.Fatal("Get with no database should fail")
	}
	if _, err := keys.List(context.Background()); err == nil {
		t.Fatal("List with no database should fail")
	}
}
