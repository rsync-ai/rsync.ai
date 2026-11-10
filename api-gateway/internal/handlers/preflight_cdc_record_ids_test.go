package handlers

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"api-gateway/internal/security"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"
)

// A CDC pipeline into an API destination that addresses each record by the id
// its API assigned (metadata.json record_ids) is refused at create
// (KI-API-DEST-CDC-WRITES-BY-SOURCE-ID). Only the batch lane keeps the
// source-row → record-id map; a CDC run wrote each change by the source row's
// own id, creating the record again or changing whichever API record carried
// that id. The orchestrator refuses the same run at start for a pipeline this
// gate never saw (cdc_record_ids_check.go).

const (
	recIDsSrc = "conn-source"
	recIDsDst = "conn-dest"
	recIDsWS  = "ws-1"
)

// recordIDsConnectors installs postgresql (source), acme-tasks (an API
// destination that declares record_ids) and mysql (a destination that does not),
// both on disk for the CapabilityAgent and in the connector index metadata is
// read from.
func recordIDsConnectors(t *testing.T) {
	t.Helper()
	installedConnectors(t, "postgresql", "acme-tasks", "mysql")
	index := t.TempDir()
	writeDiskConnector(t, index, "acme-tasks", map[string]any{
		"id": "acme-tasks", "connector_type": "acme-tasks", "category": "api_saas",
		"supports_destination": true, "record_ids": true,
	})
	writeDiskConnector(t, index, "mysql", map[string]any{
		"id": "mysql", "connector_type": "mysql", "supports_destination": true,
	})
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", index)
}

// encryptedConfig is a connection's config column as the gateway stores it.
func encryptedConfig(t *testing.T, cfg string) string {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", "unit-test-encryption-key-record-ids-0123456789")
	c, err := crypto.EncryptString(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func expectPreflightConnectorTypes(mock sqlmock.Sqlmock, src, dst, ws, destType string) {
	for _, side := range [][2]string{{src, "postgresql"}, {dst, destType}} {
		mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
			WithArgs(side[0], ws).
			WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow(side[1]))
	}
}

func expectDestConfig(mock sqlmock.Sqlmock, dst, ws, cipher, destType string) {
	mock.ExpectQuery(`SELECT config, connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(dst, ws).
		WillReturnRows(sqlmock.NewRows([]string{"config", "connector_type"}).AddRow(cipher, destType))
}

func TestPreflight_CDCIntoARecordIDsDestinationIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		destType string
		syncMode *string
		cdcMode  *string
		// srcMode is the source connection's sync_mode, read only when the
		// request names no mode.
		srcMode string
		destCfg string
		refused bool
	}{
		{name: "CDC", destType: "acme-tasks", syncMode: strptr("cdc"), destCfg: `{"api_key":"k"}`, refused: true},
		{name: "streaming", destType: "acme-tasks", syncMode: strptr("streaming"), destCfg: `{}`, refused: true},
		{name: "a cdc_mode alone", destType: "acme-tasks", cdcMode: strptr("debezium"), destCfg: `{}`, refused: true},
		{name: "CDC inherited from the source connection", destType: "acme-tasks", srcMode: "cdc", destCfg: `{}`, refused: true},
		{name: "an upsert write mode", destType: "acme-tasks", syncMode: strptr("cdc"), destCfg: `{"cdc_write_mode":"upsert"}`, refused: true},
		// Controls: each differs from a refused case in one input.
		{name: "batch", destType: "acme-tasks", syncMode: strptr("batch")},
		{name: "batch inherited from the source connection", destType: "acme-tasks", srcMode: "batch"},
		{name: "CDC into a destination without record_ids", destType: "mysql", syncMode: strptr("cdc")},
		{name: "CDC written as an append-only history", destType: "acme-tasks", syncMode: strptr("cdc"), destCfg: `{"cdc_write_mode":"Append"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recordIDsConnectors(t)
			db, mock := newMockDB(t)
			expectPreflightConnectorTypes(mock, recIDsSrc, recIDsDst, recIDsWS, tc.destType)
			if tc.destType == "acme-tasks" {
				if tc.syncMode == nil && tc.cdcMode == nil {
					mock.ExpectQuery(`SELECT sync_mode FROM connections WHERE id = \$1`).
						WithArgs(recIDsSrc).
						WillReturnRows(sqlmock.NewRows([]string{"sync_mode"}).AddRow(tc.srcMode))
				}
				if tc.destCfg != "" {
					expectDestConfig(mock, recIDsDst, recIDsWS, encryptedConfig(t, tc.destCfg), tc.destType)
				}
			}

			req := &CreatePipelineRequest{Name: "x", Request: "r", SyncMode: tc.syncMode, CDCMode: tc.cdcMode}
			src, dst := recIDsSrc, recIDsDst
			hitl, err := runPipelinePreflight(db, recIDsWS, req, &src, &dst)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			defer func() {
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Errorf("SQL: %v", err)
				}
			}()
			if !tc.refused {
				if hitl != nil {
					t.Fatalf("want the pipeline created, got HITL: %+v", hitl)
				}
				return
			}
			if hitl == nil {
				t.Fatal("a CDC pipeline into a destination that keys records by its own ids was created; want a refusal")
			}
			if hitl.Agent != "CapabilityAgent" || hitl.ActionNeeded != "use_batch_sync" ||
				hitl.Role != "destination" || hitl.ConnectorType != "acme-tasks" {
				t.Errorf("HITL = %+v, want CapabilityAgent/use_batch_sync on the destination acme-tasks", hitl)
			}
			for _, want := range []string{"acme-tasks", "as a batch sync", "cdc_write_mode=append"} {
				if !strings.Contains(hitl.Message, want) {
					t.Errorf("message %q does not name %q", hitl.Message, want)
				}
			}
		})
	}
}

// Chat passes its sync mode to the INSERT, not the create request, so the
// preflight saw every chat pipeline as batch.
func TestChatCreatePipeline_CDCIntoARecordIDsDestinationIsRefused(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	recordIDsConnectors(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectChatGatesBeforePreflight(mock)
	expectPreflightConnectorTypes(mock, chatPreflightSrc, chatPreflightDst, wsScopeWS, "acme-tasks")
	expectDestConfig(mock, chatPreflightDst, wsScopeWS, encryptedConfig(t, `{}`), "acme-tasks")

	id, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"stream pg changes to acme", wsScopeUser, "trace", chatPreflightSrc, chatPreflightDst,
		"cdc", "debezium", "", nlTransformSpec{}, true, nil, "")
	if err == nil || id != "" {
		t.Fatalf("chat CDC create into a record_ids destination: id=%q err=%v, want a refusal", id, err)
	}
	if !strings.Contains(err.Error(), "as a batch sync") {
		t.Fatalf("refusal = %q, want the preflight's reason", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// Control: the same chat create as batch reaches the INSERT without reading the
// destination's config.
func TestChatCreatePipeline_BatchIntoARecordIDsDestinationReachesInsert(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	recordIDsConnectors(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectChatGatesBeforePreflight(mock)
	expectPreflightConnectorTypes(mock, chatPreflightSrc, chatPreflightDst, wsScopeWS, "acme-tasks")
	stop := errors.New("stop at insert")
	mock.ExpectExec(`INSERT INTO pipelines`).WillReturnError(stop)

	_, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"copy pg to acme", wsScopeUser, "trace", chatPreflightSrc, chatPreflightDst,
		"batch", "", "", nlTransformSpec{}, true, nil, "")
	if err == nil || !strings.Contains(err.Error(), stop.Error()) {
		t.Fatalf("err = %v, want the INSERT to be reached", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// The words that exempt a destination are the ones the kafka-mcp-sink treats as
// append-only history (cdcAppendMode): there every change is a new record, so no
// record is addressed by the source row's id.
func TestCDCAppendWriteModes_MatchTheSink(t *testing.T) {
	path := filepath.Join("..", "..", "..", "shared", "mcp-connectors", "internal", "kafka-mcp-sink",
		"worker-src", "cmd", "kafka-sink-worker", "main.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the sink: %v", err)
	}
	m := regexp.MustCompile(`(?s)func cdcAppendMode\(.*?\n\tcase ([^\n]*):\n\t\treturn true`).FindSubmatch(src)
	if m == nil {
		t.Fatal("cdcAppendMode's case list not found in the sink")
	}
	var sink []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		sink = append(sink, string(q[1]))
	}
	got := slices.Clone(cdcAppendWriteModes)
	slices.Sort(got)
	slices.Sort(sink)
	if len(sink) == 0 || !slices.Equal(got, sink) {
		t.Fatalf("gateway append words %v, sink cdcAppendMode %v", got, sink)
	}
}
