package stmt

import (
	"regexp"
	"strings"
)

// StatementClass buckets a SQL statement by the kind of authority it needs. The
// Data Explorer gates each class on a minimum workspace role (classToMinRole).
type StatementClass string

const (
	ClassRead        StatementClass = "read"        // SELECT, WITH — unchanged read surface
	ClassDMLWrite    StatementClass = "dml_write"   // INSERT, UPDATE, DELETE, MERGE
	ClassDDL         StatementClass = "ddl"         // CREATE, ALTER
	ClassDestructive StatementClass = "destructive" // DROP, TRUNCATE
	ClassBlocked     StatementClass = "blocked"     // GRANT/REVOKE/CALL/EXEC/SET/… never allowed via Explorer
	ClassUnknown     StatementClass = "unknown"     // unrecognized → fail closed (owner-only)
)

// alterDropRe matches a DROP sub-clause inside an ALTER statement (DROP COLUMN,
// DROP CONSTRAINT, DROP PARTITION, …). Word-boundaried so ordinary identifiers such
// as `drop_reason` or `backdrop` do not trip it. Over-matching is the safe direction
// here: the cost is one extra confirmation prompt, the cost of under-matching is
// irreversible data loss on a single click.
var alterDropRe = regexp.MustCompile(`(?i)\bDROP\b`)

// alterDropsObject reports whether an ALTER statement carries a DROP sub-clause.
// `ALTER TABLE t DROP COLUMN c` destroys data exactly as irreversibly as
// `DROP TABLE t`, but its leading verb is ALTER, so a verb-only classifier files it
// as ordinary DDL — admin role, no confirmation. This is what closes that gap.
func alterDropsObject(sqlText string) bool {
	return alterDropRe.MatchString(sqlText)
}

// cteWriteVerbs maps a write verb to the class a CTE-led statement containing it
// really needs. Ordered by descending severity so the first match wins: a statement
// carrying both DROP and UPDATE must land on the stricter of the two.
//
// Word-boundaried for the same reason alterDropRe is — `delete_count`, `update_time`
// and `merge_id` are ordinary column names and must not escalate anything.
var cteWriteVerbs = []struct {
	re    *regexp.Regexp
	class StatementClass
}{
	{regexp.MustCompile(`(?i)\bDROP\b`), ClassDestructive},
	{regexp.MustCompile(`(?i)\bTRUNCATE\b`), ClassDestructive},
	{regexp.MustCompile(`(?i)\bCREATE\b`), ClassDDL},
	{regexp.MustCompile(`(?i)\bALTER\b`), ClassDDL},
	{regexp.MustCompile(`(?i)\bINSERT\b`), ClassDMLWrite},
	{regexp.MustCompile(`(?i)\bUPDATE\b`), ClassDMLWrite},
	{regexp.MustCompile(`(?i)\bDELETE\b`), ClassDMLWrite},
	{regexp.MustCompile(`(?i)\bMERGE\b`), ClassDMLWrite},
}

// cteWriteClass reports the class a WITH-led statement needs, or ClassRead if it
// writes nothing. strippedSQL must already have had removeComments applied (the
// caller has); string/identifier literals are blanked here so
// `SELECT 'MERGE INTO x' AS note` and `/* DROP TABLE t */` stay reads.
//
// Never strip comments a second time. Removing `/**/` from `1 -/**/- 1` leaves
// `1 -- 1`, and a second pass reads that as a line comment and hides whatever
// follows on the line — `WITH s AS (SELECT 1 -/**/- 1) DELETE FROM t` classified
// as a read. Pinned by shared/explorer_statement_class_golden.json.
//
// This scans the whole statement rather than parsing the CTE list, and that is a
// deliberate choice. Both shapes are dangerous — the write can sit in a CTE body
// (`WITH gone AS (DELETE … RETURNING *) SELECT * FROM gone`) or after the CTE list
// (`WITH s AS (SELECT 1) MERGE INTO …`) — and a hand-rolled CTE parser that misses a
// dialect's shape fails open, which is the failure that got us here. Over-matching
// is the safe direction, exactly as it is for alterDropRe: the cost is a viewer
// being told a query needs admin, and the cost of under-matching is a viewer
// silently writing production data on a schedule.
//
// Known over-match: `WITH s AS (…) SELECT … FOR UPDATE` is a locking read and now
// classifies as dml_write. It takes row locks, so admin is defensible, and the
// bare-SELECT form is untouched.
func cteWriteClass(strippedSQL string) StatementClass {
	scanned := stripStringLiterals(strippedSQL)
	for _, v := range cteWriteVerbs {
		if v.re.MatchString(scanned) {
			return v.class
		}
	}
	return ClassRead
}

// ClassifyStatementSQL classifies a full statement. Prefer this over
// ClassifyStatement: the leading verb alone cannot distinguish
// `ALTER TABLE t ADD COLUMN c INT` (routine DDL) from `ALTER TABLE t DROP COLUMN c`
// (irreversible), and only the second should require owner + confirmation.
func ClassifyStatementSQL(sqlText string) StatementClass {
	// Classify the statement the engine will actually run, which is this text with its
	// comments removed. identifyStatementType prefix-matches the leading verb, so an
	// ordinary `-- daily revenue\nSELECT …` matched no verb at all and fell to
	// ClassUnknown — and ClassUnknown is a write class. That had two visible faces:
	// ExecuteExplorerQuery routed a commented SELECT down the no-rows write path, and
	// authorizeModelRun refused the same SQL for "not being a read query", which
	// auto-paused its schedule. ValidateExplorerStatement had always classified on the
	// stripped text, so the two disagreed about identical SQL.
	//
	// alterDropsObject and cteWriteClass read the stripped text for the same reason in
	// the opposite direction: a DROP named only inside a comment escalated a harmless
	// ALTER to owner-only. Neither over- nor under-matching is wanted here — the
	// classifier should see exactly what the engine sees, which is what removeComments
	// now yields (it deliberately keeps MySQL `/*!` executable-comment bodies).
	stripped := strings.TrimSpace(removeComments(sqlText))
	stmtType := identifyStatementType(stripped)
	class := ClassifyStatement(stmtType)
	if class == ClassDDL && alterDropsObject(stripped) {
		return ClassDestructive
	}
	// WITH is the other verb that lies about what its statement does, and it lies in
	// the more dangerous direction: it claims to be a read. `WITH s AS (SELECT 1)
	// MERGE INTO invoices …` classified as ClassRead, which routed it to the SELECT
	// pipeline, whose dangerous-keyword list names INSERT INTO / UPDATE / DELETE FROM
	// but never named MERGE — so it ran at viewer role. Class must follow the write.
	if stmtType == "WITH" {
		if escalated := cteWriteClass(stripped); escalated != ClassRead {
			return escalated
		}
	}
	return class
}

// ClassifyStatement maps a statement type (as returned by identifyStatementType)
// to its StatementClass. EXPLAIN/SHOW/DESCRIBE and the privilege/procedural verbs
// are ClassBlocked — the read surface stays exactly SELECT/WITH (unchanged), and
// writes are opened per the role matrix.
//
// This classifies by verb only. Callers that hold the full statement text should use
// ClassifyStatementSQL so that ALTER … DROP is caught as destructive.
func ClassifyStatement(stmtType string) StatementClass {
	switch strings.ToUpper(strings.TrimSpace(stmtType)) {
	case "SELECT", "WITH":
		return ClassRead
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return ClassDMLWrite
	case "CREATE", "ALTER":
		return ClassDDL
	case "DROP", "TRUNCATE":
		return ClassDestructive
	case "GRANT", "REVOKE", "CALL", "EXEC", "EXECUTE", "SET", "COPY",
		"VACUUM", "ANALYZE", "EXPLAIN", "SHOW", "DESCRIBE", "DESC":
		return ClassBlocked
	default:
		return ClassUnknown
	}
}

// IsWriteClass reports whether a class mutates data/schema (i.e. the execution path
// must use a no-rows "execute" rather than a rows-returning "query"). Reads return
// false; every gated write class returns true.
func IsWriteClass(class StatementClass) bool {
	switch class {
	case ClassDMLWrite, ClassDDL, ClassDestructive, ClassUnknown:
		return true
	default:
		return false
	}
}

// classRank orders the classes a caller may be allowed: a read; a DML write or
// non-destructive DDL (an admin's); a destructive or unrecognized statement (an
// owner's). ClassBlocked has no rank: no caller may run it.
var classRank = map[StatementClass]int{
	ClassRead:        0,
	ClassDMLWrite:    1,
	ClassDDL:         1,
	ClassDestructive: 2,
	ClassUnknown:     2,
}

// Permits reports whether a caller allowed up to class `allowed` may run a statement
// of class `class`. The engine asks it again before a write, with the same classifier
// the gateway used, so a statement above what the caller may run is refused even if
// the gateway's own check was skipped (query-service-design.md § 4.1).
func Permits(allowed, class StatementClass) bool {
	a, okA := classRank[allowed]
	c, okC := classRank[class]
	return okA && okC && c <= a
}

// RemoveComments, HasMultipleStatements, IdentifyStatementType and ExtractTables
// export the classifier's own steps to the gateway's role check
// (validators.ValidateExplorerStatement), which runs them on the same text.
func RemoveComments(sql string) string        { return removeComments(sql) }
func HasMultipleStatements(sql string) bool   { return hasMultipleStatements(sql) }
func IdentifyStatementType(sql string) string { return identifyStatementType(sql) }
func ExtractTables(sql string) []string       { return extractTables(sql) }
