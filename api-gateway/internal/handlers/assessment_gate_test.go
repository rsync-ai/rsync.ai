package handlers

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSinkSupportsAutoCreate_ObjectStoresAreNotBlocked pins the fix for the
// false SINK_NO_DDL blocker: an object store has no tables to pre-create, so
// every MongoDB→GCS/S3/Azure-Blob pipeline was raising a BLOCKING error on
// every table that the user had no way to clear.
//
// It reads the REAL connector metadata off disk, so it fails if either half of
// the fix regresses — the Go reader or the connectors' metadata.json flags.
func TestSinkSupportsAutoCreate_ObjectStoresAreNotBlocked(t *testing.T) {
	root := filepath.Join("..", "..", "..", "shared", "mcp-connectors", "public")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("connector tree not present at %s: %v", root, err)
	}
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", root)

	cases := []struct {
		connector string
		want      bool
		why       string
	}{
		// The bug: all three answered false and blocked the run.
		{"gcs", true, "object store — writes an object per batch, nothing to pre-create"},
		{"aws-s3", true, "object store — nothing to pre-create"},
		{"azure-blob", true, "object store — nothing to pre-create"},
		// Regression guards for the cases that already worked.
		{"mongodb", true, "creates the collection on first write"},
		{"postgresql", true, "hardcoded sinksWithAutoCreate entry — issues real DDL"},
		{"mysql", true, "metadata declares supports_ddl + auto_create"},
		// Negative control: without one, a reader that returns true for
		// everything would pass every assertion above.
		{"definitely-not-a-connector", false, "unknown connector must stay fail-closed"},
	}
	for _, tc := range cases {
		if got := sinkSupportsAutoCreate(tc.connector); got != tc.want {
			t.Errorf("sinkSupportsAutoCreate(%q) = %v, want %v (%s)", tc.connector, got, tc.want, tc.why)
		}
	}
}

// TestConnectorSupportsDDL_ObjectStoresStillDeclareNoDDL guards the other side
// of the same change: the object stores must NOT start claiming real DDL. The
// kafka-sink worker gates ensure_table on supports_ddl && auto_create, so
// flipping supports_ddl to true here would make it issue CREATE TABLE calls
// against a bucket.
func TestConnectorSupportsDDL_ObjectStoresStillDeclareNoDDL(t *testing.T) {
	root := filepath.Join("..", "..", "..", "shared", "mcp-connectors", "public")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("connector tree not present at %s: %v", root, err)
	}
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", root)

	for _, ct := range []string{"gcs", "aws-s3", "azure-blob", "mongodb"} {
		if connectorSupportsDDL(ct) {
			t.Errorf("connectorSupportsDDL(%q) = true, want false", ct)
		}
	}
	// Positive control: the reader is capable of returning true at all. mysql is
	// the control rather than postgresql because postgresql's metadata.json
	// declares no supports_ddl key — it reaches sinkSupportsAutoCreate through
	// the hardcoded sinksWithAutoCreate map instead.
	if !connectorSupportsDDL("mysql") {
		t.Error("connectorSupportsDDL(\"mysql\") = false, want true — reader is broken, the assertions above are vacuous")
	}
}

// TestEvaluateAssessmentGate pins the severity boundary: ack_warnings waives
// WARNINGS and only warnings. RunPipeline used to skip the whole assessment
// when ack_warnings was set, so the flag silently waived blocking ERRORS too.
func TestEvaluateAssessmentGate(t *testing.T) {
	report := func(sev AssessmentSeverity) *AssessmentReport {
		r := &AssessmentReport{Tables: []AssessmentTable{{
			Name: "users", Findings: []AssessmentFinding{{Code: "X", Severity: sev}},
		}}}
		if sev == AssessmentError {
			r.Blocking = true
		}
		return r
	}

	cases := []struct {
		name string
		rep  *AssessmentReport
		ack  bool
		want assessmentGateOutcome
	}{
		{"nil report allows", nil, false, assessmentGateAllow},
		{"clean report allows", &AssessmentReport{}, false, assessmentGateAllow},
		{"info only allows", report(AssessmentInfo), false, assessmentGateAllow},
		{"warning without ack needs ack", report(AssessmentWarning), false, assessmentGateNeedsAck},
		{"warning with ack allows", report(AssessmentWarning), true, assessmentGateAllow},
		{"error without ack blocks", report(AssessmentError), false, assessmentGateBlocked},
		// The bug, in one line: ack must NOT clear an error.
		{"error WITH ack still blocks", report(AssessmentError), true, assessmentGateBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateAssessmentGate(tc.rep, tc.ack, nil); got != tc.want {
				t.Errorf("evaluateAssessmentGate(ack=%v) = %v, want %v", tc.ack, got, tc.want)
			}
		})
	}

	// An ERROR finding blocks even if Blocking was somehow left false.
	r := report(AssessmentError)
	r.Blocking = false
	if got := evaluateAssessmentGate(r, true, nil); got != assessmentGateBlocked {
		t.Errorf("error finding with Blocking=false = %v, want blocked", got)
	}
}

// TestSinkSupportsAutoCreate_APIDestinationsAreNotBlocked — the same false
// SINK_NO_DDL blocker, for a REST/GraphQL API destination (P5, live: a re-run
// of a Sample Data → ClickUp pipeline was refused before it started, "cannot
// auto-create … tables. Create them manually first"). An API has resources, not
// tables: there is nothing the user can go and pre-create, so the stop cannot be
// cleared. Whether the named resource takes writes is checked when the sink
// starts (executor destResourceNotWritable), against what the connector says
// it can write.
//
// stripe and shopify-admin-graphql are shipped API destinations that declare
// supports_ddl=false and auto_create_destination_tables=false, as every
// generated REST connector does; the temp connector is that generated shape.
func TestSinkSupportsAutoCreate_APIDestinationsAreNotBlocked(t *testing.T) {
	root := t.TempDir()
	writeConnector := func(id, category string) {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(filepath.Join(dir, "versions", "v1.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		latest := `{"current_version": "v1.0.0", "all_versions": ["v1.0.0"]}`
		meta := `{"id": "` + id + `", "connector_type": "` + id + `", "category": "` + category + `",
		  "supports_ddl": false, "auto_create_destination_tables": false,
		  "capabilities": {"supports_destination": true, "supports_ddl": false, "auto_create_destination_tables": false}}`
		if err := os.WriteFile(filepath.Join(dir, "latest.json"), []byte(latest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "versions", "v1.0.0", "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConnector("t-api-dest", "api_saas")
	// Control: the same flags on a database. A database without DDL has tables
	// the user must create, so it stays blocked — the rule is the category, not
	// "any connector with metadata".
	writeConnector("t-db-no-ddl", "relational_db")
	for _, real := range []string{"stripe", "shopify-admin-graphql"} {
		src := filepath.Join("..", "..", "..", "shared", "mcp-connectors", "public", real)
		if _, err := os.Stat(src); err != nil {
			t.Skipf("connector tree not present at %s: %v", src, err)
		}
		copyConnectorMetadata(t, src, filepath.Join(root, real))
	}
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", root)

	for _, tc := range []struct {
		connector string
		want      bool
	}{
		{"t-api-dest", true},
		{"stripe", true},
		{"shopify-admin-graphql", true},
		{"t-db-no-ddl", false},
		{"definitely-not-a-connector", false},
	} {
		if got := sinkSupportsAutoCreate(tc.connector); got != tc.want {
			t.Errorf("sinkSupportsAutoCreate(%q) = %v, want %v", tc.connector, got, tc.want)
		}
	}
	// The other side: an API destination must not start claiming DDL, or the
	// sink (probeDDLSupport: supports_ddl && auto_create) would send it CREATE TABLE.
	for _, ct := range []string{"t-api-dest", "stripe", "shopify-admin-graphql"} {
		if connectorSupportsDDL(ct) {
			t.Errorf("connectorSupportsDDL(%q) = true, want false", ct)
		}
	}
}

// copyConnectorMetadata copies a connector's latest.json and the metadata.json of
// its current version — the files the connector index reads — so a test root can
// hold real connectors beside made-up ones (WalkDir does not follow symlinks).
func copyConnectorMetadata(t *testing.T, src, dst string) {
	t.Helper()
	latest, err := os.ReadFile(filepath.Join(src, "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	ver := readCurrentVersionFromLatestJSON(src)
	meta, err := os.ReadFile(filepath.Join(src, "versions", ver, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "versions", ver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "latest.json"), latest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "versions", ver, "metadata.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
}
