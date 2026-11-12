package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"
)

// The table preview in the pipeline wizard answered every failure with 502 and
// the reason in `details`. app.rsync.ai sits behind Cloudflare, which swaps an
// origin 502 or 504 for its own HTML page, so the browser got that page and the
// panel showed "HTTP 502" with no reason. Each failure must come back with a
// status Cloudflare passes through, still carrying `details`.

const sampleConnectorRefusal = "Missing scope read_orders: grant it in the Shopify app settings."

func callSample(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := wsScopeRouter(http.MethodGet, "/api/v1/connections/:id/sample", SampleConnectionData)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+wsScopeConnection+"/sample?table=orders&limit=5", nil))
	return rr
}

func expectSampleConnectionRead(mock sqlmock.Sqlmock, config string) {
	mock.ExpectQuery(`SELECT connector_type, config, connector_version\s+FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(wsScopeConnection, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config", "connector_version"}).AddRow("shopify", config, nil))
}

func assertSampleFailure(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantDetails string) {
	t.Helper()
	if rr.Code == http.StatusBadGateway || rr.Code == http.StatusGatewayTimeout {
		t.Fatalf("status = %d: Cloudflare replaces this body, so the reason never reaches the browser", rr.Code)
	}
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, wantStatus, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if body["error"] == "" || body["error"] == nil {
		t.Fatalf("no error label: %s", rr.Body.String())
	}
	if wantDetails != "" && body["details"] != wantDetails {
		t.Fatalf("details = %q, want %q", body["details"], wantDetails)
	}
	if wantDetails == "" && body["details"] == nil {
		t.Fatalf("no details: %s", rr.Body.String())
	}
}

// The connector refused: the preview depends on it and it failed, so 424 with
// the connector's own words.
func TestSampleConnectionData_ConnectorRefusalPassesThroughCloudflare(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"shop":"example","access_token":"unit-test-token"}`)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	calls, _ := docFindOrchestrator(t, "/api/v1/agent/sample-rows", http.StatusInternalServerError,
		`{"error":"sample_rows_failed","details":"`+sampleConnectorRefusal+`"}`)

	expectSampleConnectionRead(mock, enc)
	mock.ExpectExec(`INSERT INTO connection_access_logs`).WillReturnResult(sqlmock.NewResult(1, 1))

	rr := callSample(t)

	if calls.Load() != 1 {
		t.Fatalf("orchestrator calls = %d, want 1", calls.Load())
	}
	assertSampleFailure(t, rr, http.StatusFailedDependency, sampleConnectorRefusal)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// The gateway's own failures -- reading the connection, decrypting its
// credentials -- are a 500, which Cloudflare also passes through.
func TestSampleConnectionData_GatewayFailuresPassThroughCloudflare(t *testing.T) {
	t.Run("connection read fails", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(`SELECT connector_type, config, connector_version`).
			WillReturnError(errors.New("conn reset"))

		assertSampleFailure(t, callSample(t), http.StatusInternalServerError, "")
	})
	t.Run("credentials do not decrypt", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
		expectSampleConnectionRead(mock, "not-a-ciphertext")

		assertSampleFailure(t, callSample(t), http.StatusInternalServerError, "")
	})
}
