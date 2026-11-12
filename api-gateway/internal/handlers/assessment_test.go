package handlers

import (
	"strings"
	"testing"
)

// Helper: gather a slice of finding codes by severity for easy assertions.
func findingCodes(t AssessmentTable, sev AssessmentSeverity) []string {
	out := []string{}
	for _, f := range t.Findings {
		if f.Severity == sev {
			out = append(out, f.Code)
		}
	}
	return out
}

func TestAssessTable_DeclaredPKSinkSupportsDDL(t *testing.T) {
	// Happy path: shopify-like source with declared PK + sink supports auto-create.
	// Expectation: zero errors, zero warnings, only JSON-collapse info if applicable.
	meta := TableMetadata{
		Name:        "products",
		Schema:      "shopify",
		PrimaryKeys: []string{"id"},
		Columns: []ColumnMetadata{
			{Name: "id", Type: "string"},
			{Name: "title", Type: "string"},
			{Name: "priceRangeV2", Type: "json"},
			{Name: "variants", Type: "json"},
			{Name: "totalInventory", Type: "integer"},
		},
	}
	got := assessTable(meta, true, nil)

	if got.PrimaryKeySource != "declared" {
		t.Errorf("primary_key_source = %q, want declared", got.PrimaryKeySource)
	}
	if got.Mode != "upsert" {
		t.Errorf("mode = %q, want upsert", got.Mode)
	}
	if codes := findingCodes(got, AssessmentError); len(codes) != 0 {
		t.Errorf("unexpected errors: %v", codes)
	}
	if codes := findingCodes(got, AssessmentWarning); len(codes) != 0 {
		t.Errorf("unexpected warnings: %v", codes)
	}
	// JSON collapse for the 2 json columns
	if got.JSONColumnCount != 2 {
		t.Errorf("json_column_count = %d, want 2", got.JSONColumnCount)
	}
	if codes := findingCodes(got, AssessmentInfo); len(codes) != 1 || codes[0] != FindingJSONCollapse {
		t.Errorf("info codes = %v, want [JSON_COLLAPSE]", codes)
	}
}

func TestAssessTable_NoPKSinkSupportsDDL_WarningOnly(t *testing.T) {
	// PK-less source but sink can DDL → synthetic PK warning (not error).
	meta := TableMetadata{
		Name:        "events",
		PrimaryKeys: nil,
		Columns: []ColumnMetadata{
			{Name: "ts", Type: "timestamp"},
			{Name: "payload", Type: "string"},
		},
	}
	got := assessTable(meta, true, nil)

	if got.PrimaryKeySource != "synthetic" {
		t.Errorf("primary_key_source = %q, want synthetic", got.PrimaryKeySource)
	}
	if got.Mode != "upsert_synthetic" {
		t.Errorf("mode = %q, want upsert_synthetic", got.Mode)
	}
	warns := findingCodes(got, AssessmentWarning)
	if len(warns) != 1 || warns[0] != FindingNoPrimaryKey {
		t.Errorf("warning codes = %v, want [NO_PRIMARY_KEY]", warns)
	}
	if errs := findingCodes(got, AssessmentError); len(errs) != 0 {
		t.Errorf("unexpected errors when sink supports DDL: %v", errs)
	}
}

func TestAssessTable_NoPKNoDDL_WarnPlusSinkError(t *testing.T) {
	// PK-less source AND sink can't auto-create. Per the "warn, never block"
	// market-standard policy, NO_PRIMARY_KEY is a WARNING (the run can still
	// start and the sink fails loud rather than silently dropping rows), while
	// SINK_NO_DDL remains an ERROR (the destination genuinely can't be created).
	meta := TableMetadata{
		Name:        "events",
		PrimaryKeys: nil,
		Columns: []ColumnMetadata{
			{Name: "ts", Type: "timestamp"},
		},
	}
	got := assessTable(meta, false, nil)

	warns := findingCodes(got, AssessmentWarning)
	hasPKWarn := false
	for _, c := range warns {
		if c == FindingNoPrimaryKey {
			hasPKWarn = true
		}
	}
	if !hasPKWarn {
		t.Errorf("warning codes = %v, want NO_PRIMARY_KEY as a warning", warns)
	}
	errs := findingCodes(got, AssessmentError)
	hasDDL := false
	for _, c := range errs {
		if c == FindingSinkNoDDL {
			hasDDL = true
		}
		if c == FindingNoPrimaryKey {
			t.Errorf("NO_PRIMARY_KEY must be a warning, not an error (warn-never-block policy)")
		}
	}
	if !hasDDL {
		t.Errorf("error codes = %v, want SINK_NO_DDL", errs)
	}
}

func TestAssessTable_NominatedKeysSuppressesWarning(t *testing.T) {
	// PR-D: a keyless source with user-nominated key columns is treated as
	// keyed — INFO note, no warning, true upsert mode (not synthetic hash).
	meta := TableMetadata{
		Name:        "events",
		Schema:      "public",
		PrimaryKeys: nil,
		Columns: []ColumnMetadata{
			{Name: "tenant_id", Type: "string"},
			{Name: "event_id", Type: "string"},
			{Name: "payload", Type: "string"},
		},
	}
	got := assessTable(meta, true, []string{"tenant_id", "event_id"})

	if got.PrimaryKeySource != "nominated" {
		t.Errorf("primary_key_source = %q, want nominated", got.PrimaryKeySource)
	}
	if got.Mode != "upsert" {
		t.Errorf("mode = %q, want upsert", got.Mode)
	}
	if len(got.NominatedKeys) != 2 || got.NominatedKeys[0] != "tenant_id" || got.NominatedKeys[1] != "event_id" {
		t.Errorf("nominated_keys = %v, want [tenant_id event_id]", got.NominatedKeys)
	}
	if warns := findingCodes(got, AssessmentWarning); len(warns) != 0 {
		t.Errorf("unexpected warnings when keys nominated: %v", warns)
	}
	// The keyless finding is downgraded to INFO and names the nominated columns.
	infos := findingCodes(got, AssessmentInfo)
	hasNoPKInfo := false
	for _, c := range infos {
		if c == FindingNoPrimaryKey {
			hasNoPKInfo = true
		}
	}
	if !hasNoPKInfo {
		t.Errorf("info codes = %v, want a NO_PRIMARY_KEY note", infos)
	}
	// Columns are exposed so the modal can render the picker.
	if len(got.Columns) != 3 {
		t.Errorf("columns = %v, want 3 source columns", got.Columns)
	}
}

func TestAssessTable_JSONCollapseDetailsRecordColumnNames(t *testing.T) {
	meta := TableMetadata{
		Name:        "orders",
		PrimaryKeys: []string{"id"},
		Columns: []ColumnMetadata{
			{Name: "id", Type: "string"},
			{Name: "totalPriceSet", Type: "json"},
			{Name: "lineItems", Type: "json"},
			{Name: "shippingAddress", Type: "json"},
		},
	}
	got := assessTable(meta, true, nil)
	infos := got.Findings // we expect exactly one info
	if len(infos) != 1 || infos[0].Code != FindingJSONCollapse {
		t.Fatalf("findings = %v, want a single JSON_COLLAPSE info", infos)
	}
	cols, _ := infos[0].Details["columns"].([]string)
	if len(cols) != 3 {
		t.Errorf("json_collapse columns = %v, want 3 entries", cols)
	}
}

// PostgreSQL discovery reports a NUMERIC with no precision or scale as the bare
// "numeric" (shared/unconstrained_numeric_golden.json). The run warns, says where
// the value lands, and hands over the ALTER that gives the column a fixed width.
func TestAssessTable_UnconstrainedNumericWarnsWithTheAlter(t *testing.T) {
	meta := TableMetadata{
		Name:        "ledger",
		Schema:      "billing",
		PrimaryKeys: []string{"id"},
		Columns: []ColumnMetadata{
			{Name: "id", Type: "integer"},
			{Name: "rate", Type: "numeric"},
			{Name: "price", Type: "numeric(12,2)"},
			{Name: "fee", Type: "decimal"},
			{Name: `odd"name`, Type: " NUMERIC "},
		},
	}
	finding := func(tbl AssessmentTable) AssessmentFinding {
		for _, x := range tbl.Findings {
			if x.Code == FindingUnconstrainedNumeric {
				return x
			}
		}
		return AssessmentFinding{}
	}
	got := assessTable(meta, true, nil)
	if codes := findingCodes(got, AssessmentWarning); len(codes) != 1 || codes[0] != FindingUnconstrainedNumeric {
		t.Fatalf("warnings = %v, want [%s]", codes, FindingUnconstrainedNumeric)
	}
	f := finding(got)
	cols, _ := f.Details["columns"].([]string)
	if strings.Join(cols, ",") != `rate,odd"name` {
		t.Errorf("columns = %v, want only the unconstrained ones, in source order", cols)
	}
	sql, _ := f.Details["sql_to_run"].([]string)
	want := []string{
		`ALTER TABLE "billing"."ledger" ALTER COLUMN "rate" TYPE numeric(p,s);`,
		`ALTER TABLE "billing"."ledger" ALTER COLUMN "odd""name" TYPE numeric(p,s);`,
	}
	if strings.Join(sql, "\n") != strings.Join(want, "\n") {
		t.Errorf("sql_to_run = %q, want %q", sql, want)
	}
	if steps, _ := f.Details["steps"].([]string); len(steps) == 0 {
		t.Error("steps missing: the modal renders them as the fix")
	}
	for _, s := range []string{"rate", "PostgreSQL", "NUMERIC", "text", "already exists"} {
		if !strings.Contains(f.Message, s) {
			t.Errorf("message %q does not mention %q", f.Message, s)
		}
	}
	// The ack key hashes the message, so it must not change between runs.
	if again := assessTable(meta, true, nil); assessmentWarningKey(again, finding(again)) != assessmentWarningKey(got, f) {
		t.Error("the warning's ack key changed between two identical runs")
	}

	// A table with no unconstrained numeric has no such finding.
	meta.Columns = meta.Columns[:4]
	meta.Columns[1].Type = "numeric(10,4)"
	if codes := findingCodes(assessTable(meta, true, nil), AssessmentWarning); len(codes) != 0 {
		t.Errorf("warnings = %v, want none", codes)
	}
}

func TestHasBlockingFindings(t *testing.T) {
	clean := []AssessmentTable{{Findings: []AssessmentFinding{{Severity: AssessmentInfo, Code: "X"}}}}
	if hasBlockingFindings(clean) {
		t.Error("info-only assessment should not block")
	}
	warned := []AssessmentTable{{Findings: []AssessmentFinding{{Severity: AssessmentWarning, Code: "Y"}}}}
	if hasBlockingFindings(warned) {
		t.Error("warnings should not block (frontend handles them via ack)")
	}
	bad := []AssessmentTable{{Findings: []AssessmentFinding{{Severity: AssessmentError, Code: "Z"}}}}
	if !hasBlockingFindings(bad) {
		t.Error("errors must block")
	}
}

func TestSummariseCountsOnlySourceTables(t *testing.T) {
	// Six real tables plus the two check rows the report appends read
	// "across 8 tables" on prod (U-19).
	rows := []AssessmentTable{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}, {Name: "f"},
		{Name: "(source readiness)"},
		{Name: "(destination namespace)", Findings: []AssessmentFinding{{Severity: AssessmentWarning, Code: "W"}}},
	}
	if got := summarise(rows); got != "1 warning across 6 tables" {
		t.Errorf("summary = %q; want %q", got, "1 warning across 6 tables")
	}
	onlyChecks := []AssessmentTable{{Name: "(catalog)", Findings: []AssessmentFinding{{Severity: AssessmentError, Code: "E"}}}}
	if got := summarise(onlyChecks); got != "1 error" {
		t.Errorf("summary = %q; want %q (no \"across 0 tables\")", got, "1 error")
	}
	if got := summarise([]AssessmentTable{{Name: "a"}}); got != "All checks passed across 1 table" {
		t.Errorf("summary = %q", got)
	}
}

func TestSummarise(t *testing.T) {
	clean := []AssessmentTable{{Name: "a"}, {Name: "b"}}
	if got := summarise(clean); got != "All checks passed across 2 tables" {
		t.Errorf("clean summary = %q", got)
	}

	mixed := []AssessmentTable{
		{Name: "a", Findings: []AssessmentFinding{
			{Severity: AssessmentWarning, Code: "W"},
			{Severity: AssessmentInfo, Code: "I"},
		}},
		{Name: "b", Findings: []AssessmentFinding{
			{Severity: AssessmentError, Code: "E"},
		}},
	}
	got := summarise(mixed)
	// Don't assert exact string — pluralisation rules can drift. Just
	// make sure the counts are surfaced.
	for _, want := range []string{"1 error", "1 warning", "1 note", "2 tables"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}
