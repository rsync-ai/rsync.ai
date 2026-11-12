package security

import (
	"reflect"
	"strings"
	"testing"
)

// The redactor runs over every string in every event payload before it is
// stored (projector/event_projector.go:608) and before it is broadcast over
// websockets (websocket/kafka_bridge.go:188 and :233). It stores the redacted
// copy — the original is not kept anywhere — so a false positive is permanent:
// the operator-facing message is gone from the UI and from the database.
//
// F-277: `looksLikeSQL` (redaction.go:107) was true on a bare "update " /
// "delete " / "select ", and `looksLikePython` (:118) on a bare "from " /
// "import " / "class ". `redactSQL` / `redactPython` then replace the ENTIRE
// string when it also contains password|secret|token|api_key. Both conditions
// are met by ordinary English.
//
// Measured on prod 2026-08-05, before the fix: 0 of 1614 stored payloads
// contain any of these placeholders, against a working positive control (1252
// of 1614 contain "message"). This is a trap being closed before it fires, not
// damage being repaired — only 3 STAGE_FAILED events exist, so the error path
// that produces these strings is barely exercised.

const (
	sqlPlaceholder    = "[SQL query redacted - contains sensitive patterns]"
	pythonPlaceholder = "[Python code redacted - contains sensitive patterns]"
)

// Ordinary English an operator needs to read. Each one contains a bare SQL or
// Python keyword AND a word from the sensitive list, which is all the old
// heuristic required to replace the whole sentence with a placeholder that also
// lies about what the string was.
func TestRedactString_OperatorProseIsNotMistakenForCode(t *testing.T) {
	cases := []struct{ name, in string }{
		// The exact shape built at backend-orchestrator/internal/workers/executor.go:533.
		{"update+token", "Data transfer failed: could not update orders: token expired"},
		{"delete+api_key", "Cannot delete pipeline: an api_key is still attached"},
		{"from+token", "Waiting for a token from the identity provider"},
		{"import+secret", "Could not import the secret store configuration"},
		{"select+password", "Select a destination before saving the password policy"},
		{"class+credentials", "This plan class does not include credentials management"},
		// Keyword pairs that co-occur in prose the way they do in a query.
		{"select..from+token", "Could not select rows from the source table: token expired"},
		{"delete..from+token", "Delete from the list any API token you no longer need"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactString(tc.in); got != tc.in {
				t.Fatalf("operator message was rewritten\n   in: %q\n  out: %q", tc.in, got)
			}
		})
	}
}

// The bound on the fix above: narrowing detection must not switch redaction
// off. Real statements carrying a credential keyword must still be replaced.
func TestRedactString_GenuineCodeWithCredentialKeywordsStaysRedacted(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"select", "SELECT id, password_hash FROM users WHERE id = 1", sqlPlaceholder},
		{"update", "UPDATE users SET password_hash = 'x' WHERE id = 1", sqlPlaceholder},
		{"delete", "DELETE FROM sessions WHERE token = 'abc'", sqlPlaceholder},
		{"insert", "INSERT INTO creds (api_key) VALUES ('sk-live-1')", sqlPlaceholder},
		{"create", "CREATE TABLE secrets (token text)", sqlPlaceholder},
		{"lowercase select", "select api_key from connections where id = 1", sqlPlaceholder},
		{"import", "import os\napi_key = os.environ['SECRET']", pythonPlaceholder},
		{"def", "def connect():\n    password = 'hunter2'", pythonPlaceholder},
		{"from-import", "from creds import password", pythonPlaceholder},
		{"class", "class Vault:\n    secret = 'x'", pythonPlaceholder},
		{"indented def", "    def login(self):\n        token = 'x'", pythonPlaceholder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactString(tc.in); got != tc.want {
				t.Fatalf("code was not redacted\n   in: %q\n  out: %q\n want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// Pre-existing contract, restated so the narrowing does not quietly change it:
// a statement with no credential keyword is recognised as code and passed
// through untouched.
func TestRedactString_HarmlessSQLIsRecognisedAndLeftAlone(t *testing.T) {
	for _, in := range []string{
		"SELECT id FROM orders",
		"DELETE FROM staging_rows WHERE id < 100",
	} {
		if got := redactString(in); got != in {
			t.Fatalf("harmless query was rewritten\n   in: %q\n  out: %q", in, got)
		}
	}
}

// redactPII's phone rule fires on any string with 10+ digits and fewer than 20
// characters that contains one of "()-. ". A Postgres text timestamp is exactly
// that: "2026-08-05 13:41:52" is 19 characters with 14 digits and a hyphen.
func TestRedactPII_PhoneRuleDoesNotEatTimestampsAndIDs(t *testing.T) {
	for _, in := range []string{
		"2026-08-05 13:41:52",
		"2026-08-05T13:41:52",
		"v1.2.3-20260805.14",
	} {
		if got := redactString(in); got != in {
			t.Fatalf("non-PII value was rewritten\n   in: %q\n  out: %q", in, got)
		}
	}
}

// The bound on that one: things that really are a run of digits behind
// phone-shaped punctuation must still be masked.
func TestRedactPII_RealPhoneShapesStayMasked(t *testing.T) {
	for _, in := range []string{
		"(555) 123-4567",
		"555-123-4567",
		"+1 555 123 4567",
		"4111111111111111",
	} {
		if got := redactString(in); got != "[phone redacted]" {
			t.Fatalf("phone-shaped value was not masked\n   in: %q\n  out: %q", in, got)
		}
	}
}

// A DSN with inline credentials matches none of the "password=" / "pwd=" /
// "secret=" / "token=" patterns that route a string to MaskConnectionString, so
// the *email* branch of redactPII is the only thing between that password and
// the event store. That is why the email rule is deliberately left broad here
// while the phone rule is narrowed — tightening it would leak the credential.
// Locked so a later tightening has to confront the consequence first.
func TestRedactString_DSNWithInlineCredentialsNeverLeaksThePassword(t *testing.T) {
	in := "postgres://svc:hunter2@db.internal:5432/demo"
	got := redactString(in)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("DSN password survived redaction\n   in: %q\n  out: %q", in, got)
	}
}

// KI-REDACTION-SQL-VERB-PROSE-RESIDUAL: a field whose producer declares it SQL
// is redacted by rule, not by the shape-and-keyword guess free text gets. The
// adapter's PIPELINE_WAITING approval card carries the schema-repair DDL twice
// (nl_pipeline_v2_workflow.go: `proposed_ddl`, and `repair_plan.ddl` from the
// SchemaRepairPlan struct), built at nl_pipeline_v2_activities.go as
// `ALTER TABLE "<s>"."<t>" ADD COLUMN IF NOT EXISTS "<col>" <type>`.
func TestRedactMap_DeclaredSQLFieldIsRedactedByRuleNotByGuess(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		path []string // where the value lands in the output
		want interface{}
	}{
		// The guess replaced the whole statement because a column NAME contains
		// "token": the approver's DDL became a placeholder, and no value was
		// ever in it.
		{
			"ddl naming a token column stays readable",
			map[string]interface{}{"proposed_ddl": []interface{}{
				`ALTER TABLE "public"."users" ADD COLUMN IF NOT EXISTS "api_token" text`,
			}},
			[]string{"proposed_ddl"},
			[]interface{}{`ALTER TABLE "public"."users" ADD COLUMN IF NOT EXISTS "api_token" text`},
		},
		// Nested: SchemaRepairPlan.DDL arrives as repair_plan.ddl.
		{
			"repair_plan.ddl is declared too",
			map[string]interface{}{"repair_plan": map[string]interface{}{"ddl": []interface{}{
				`ALTER TABLE "public"."sessions" ADD COLUMN IF NOT EXISTS "refresh_tok" text`,
			}}},
			[]string{"repair_plan", "ddl"},
			[]interface{}{`ALTER TABLE "public"."sessions" ADD COLUMN IF NOT EXISTS "refresh_tok" text`},
		},
		// The guess let a literal VALUE through whenever no keyword from its
		// short list was present: here an email address.
		{
			"a literal value never survives a declared field",
			map[string]interface{}{"sql": `UPDATE users SET email = 'ana@example.com' WHERE id = 7`},
			[]string{"sql"},
			`UPDATE users SET email = '********' WHERE id = 7`,
		},
		// And it let through any statement outside its six shapes.
		{
			"a statement the shape guess does not know is still masked",
			map[string]interface{}{"ddl": `CREATE USER etl WITH PASSWORD 'hunter2'`},
			[]string{"ddl"},
			`CREATE USER etl WITH PASSWORD '********'`,
		},
		{
			"dollar quotes, comments and an unterminated literal",
			map[string]interface{}{"ddl_applied": "SELECT $$sk-live-1$$ -- token sk-live-2\n/* sk-live-3 */ 'sk-live-4"},
			[]string{"ddl_applied"},
			"SELECT $$********$$ -- ********\n/* ******** */ '********'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got interface{} = RedactMap(tc.in)
			for _, k := range tc.path {
				got = got.(map[string]interface{})[k]
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("declared SQL field\n  got: %#v\n want: %#v", got, tc.want)
			}
		})
	}
}

// The bound: a key outside the declared list keeps the free-text backstop, and
// a sensitive key still wins over a declared one.
func TestRedactMap_UndeclaredTextKeepsTheBackstop(t *testing.T) {
	out := RedactMap(map[string]interface{}{
		"error_message": "DELETE FROM sessions WHERE token = 'abc'",
		"sql_password":  "SELECT 1",
	})
	if got := out["error_message"]; got != sqlPlaceholder {
		t.Fatalf("free-text backstop changed: %q", got)
	}
	if got := out["sql_password"]; got != MaskedValue {
		t.Fatalf("sensitive key no longer wins: %q", got)
	}
}
