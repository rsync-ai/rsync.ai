package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rsync-ai/shared/crypto"
)

const warehouseConnID = "44444444-4444-4444-4444-444444444444"

// warehouseOrchestrator answers each discover-schema call with respond(n, body),
// n counting from 0, and keeps every request body it was sent.
func warehouseOrchestrator(t *testing.T, respond func(n int, req map[string]interface{}) (int, string)) func() []map[string]interface{} {
	t.Helper()
	var mu sync.Mutex
	var sent []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agent/discover-schema" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		n := len(sent)
		sent = append(sent, req)
		mu.Unlock()
		status, body := respond(n, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ORCHESTRATOR_URL", srv.URL)
	return func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), sent...)
	}
}

func encryptedWarehouseConfig(t *testing.T, config string) string {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(config)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return enc
}

const fintechWarehouseTables = `{"overall_status":"success","warnings_messages":[],"tables":[
	{"name":"customers","schema":"fintech_silver","row_count":1234,"columns":[
		{"name":"customer_id","type":"integer","source_type":"INT64","nullable":false},
		{"name":"balance","type":"decimal","source_type":"NUMERIC","nullable":true}]},
	{"name":"daily_card_volume","schema":"fintech_gold","row_count":31,"columns":[
		{"name":"day","type":"date","source_type":"DATE","nullable":true}]},
	{"name":"_rsync_cdc_state","schema":"fintech_bronze","columns":[]}]}`

// A BigQuery connection names one dataset, but the Explorer's panel is the
// warehouse: it lists every dataset the connection can see, with each table's
// columns (in the warehouse's own type names) and row count. Before this, every
// delegated warehouse got an empty "unsupported" index and the panel read
// "0 Schemas · 0 Tables".
func TestBuildSchemaIndex_WarehouseListsEveryDatasetWithColumns(t *testing.T) {
	enc := encryptedWarehouseConfig(t, `{"project_id":"fintech-demo","dataset_id":"fintech_bronze","location":"US"}`)
	sent := warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) {
		return http.StatusOK, fintechWarehouseTables
	})

	idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", "bigquery", enc, false)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	if idx.SchemaHash == "" || idx.SchemaHash == "unsupported" {
		t.Fatalf("schema hash = %q, want a real index", idx.SchemaHash)
	}
	if idx.TableCount != 2 || len(idx.Tables) != 2 {
		t.Fatalf("tables = %+v, want customers and daily_card_volume (the _rsync table dropped)", idx.Tables)
	}
	customers := idx.Tables[0]
	if customers.Name != "customers" || customers.Schema != "fintech_silver" || customers.RowCount != 1234 {
		t.Fatalf("customers = %+v", customers)
	}
	if len(customers.Columns) != 2 {
		t.Fatalf("customers columns = %+v", customers.Columns)
	}
	if c := customers.Columns[0]; c.Name != "customer_id" || c.Type != "INT64" || c.IsNullable {
		t.Errorf("customer_id = %+v, want type INT64 (the warehouse's name) and not nullable", c)
	}
	if c := customers.Columns[1]; c.Type != "NUMERIC" || !c.IsNullable {
		t.Errorf("balance = %+v, want type NUMERIC, nullable", c)
	}
	if gold := idx.Tables[1]; gold.Schema != "fintech_gold" || gold.RowCount != 31 {
		t.Errorf("daily_card_volume = %+v", gold)
	}
	if len(customers.SearchTokens) == 0 {
		t.Errorf("customers has no search tokens, so the panel's search cannot find it")
	}

	reqs := sent()
	if len(reqs) != 1 {
		t.Fatalf("discover calls = %d, want 1", len(reqs))
	}
	req := reqs[0]
	config, _ := req["config"].(map[string]interface{})
	if config["dataset_id"] != "" {
		t.Errorf("config.dataset_id = %v, want blank so every dataset is listed", config["dataset_id"])
	}
	if config["project_id"] != "fintech-demo" || config["location"] != "US" {
		t.Errorf("config lost the connection's other settings: %v", config)
	}
	for key, want := range map[string]interface{}{
		"connector_type":        "bigquery",
		"connection_id":         warehouseConnID,
		"include_columns":       true,
		"include_row_counts":    true,
		"include_table_details": true,
		"max_tables":            float64(warehouseSchemaMaxTables),
	} {
		if req[key] != want {
			t.Errorf("request %s = %v, want %v", key, req[key], want)
		}
	}
	if warehouseSchemaMaxTables != 200 {
		t.Errorf("warehouseSchemaMaxTables = %d, want 200", warehouseSchemaMaxTables)
	}
}

// When the wider listing fails (a service account that may read its dataset but
// not list the project's), the panel still shows the connection's own dataset.
func TestBuildSchemaIndex_WarehouseFallsBackToItsOwnDataset(t *testing.T) {
	enc := encryptedWarehouseConfig(t, `{"project_id":"fintech-demo","dataset_id":"fintech_bronze"}`)
	sent := warehouseOrchestrator(t, func(n int, _ map[string]interface{}) (int, string) {
		if n == 0 {
			return http.StatusOK, `{"overall_status":"failed","tables":[],"warnings_messages":["BigQuery discovery failed: 403 Access Denied: Project fintech-demo"]}`
		}
		return http.StatusOK, `{"overall_status":"success","tables":[{"name":"card_transactions","schema":"fintech_bronze","columns":[]}]}`
	})

	idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", "bigquery", enc, false)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	if idx.TableCount != 1 || idx.Tables[0].Schema != "fintech_bronze" {
		t.Fatalf("index = %+v, want the connection's own dataset", idx.Tables)
	}
	reqs := sent()
	if len(reqs) != 2 {
		t.Fatalf("discover calls = %d, want the wide listing then the pinned one", len(reqs))
	}
	if cfg, _ := reqs[1]["config"].(map[string]interface{}); cfg["dataset_id"] != "fintech_bronze" {
		t.Errorf("fallback config.dataset_id = %v, want fintech_bronze", cfg["dataset_id"])
	}
}

// A warehouse that cannot be listed at all is an error with the connector's
// reason, not an empty panel that looks like a warehouse with no tables.
func TestBuildSchemaIndex_WarehouseFailureIsAnError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"failed status", http.StatusOK,
			`{"overall_status":"failed","tables":[],"warnings_messages":["Could not list tables in dataset fintech_bronze: 403 Access Denied"]}`,
			[]string{"could not list tables", "403 Access Denied"}},
		{"orchestrator error", http.StatusUnprocessableEntity,
			`{"error":"discovery failed","details":"Missing project_id; cannot list tables."}`,
			[]string{"status 422", "Missing project_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc := encryptedWarehouseConfig(t, `{"project_id":"fintech-demo","dataset_id":"fintech_bronze"}`)
			warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) { return tc.status, tc.body })

			idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", "bigquery", enc, false)
			if err == nil {
				t.Fatalf("buildSchemaIndex = %+v, want an error", idx)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// ClickHouse, Snowflake and Oracle name a database or schema, not a dataset: their
// configuration goes to the connector as it is, in one call.
func TestBuildSchemaIndex_OtherWarehousesKeepTheirConfig(t *testing.T) {
	for _, connectorType := range []string{"clickhouse", "snowflake", "oracle"} {
		t.Run(connectorType, func(t *testing.T) {
			enc := encryptedWarehouseConfig(t, `{"host":"wh.internal","database":"analytics","schema":"PUBLIC"}`)
			sent := warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) {
				return http.StatusOK, `{"overall_status":"success","tables":[{"name":"events","schema":"analytics","row_count":7,"columns":[{"name":"id","type":"integer","source_type":"UInt64"}]}]}`
			})

			idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", connectorType, enc, false)
			if err != nil {
				t.Fatalf("buildSchemaIndex: %v", err)
			}
			if idx.TableCount != 1 || idx.Tables[0].Columns[0].Type != "UInt64" || idx.Tables[0].RowCount != 7 {
				t.Fatalf("index = %+v", idx.Tables)
			}
			reqs := sent()
			if len(reqs) != 1 {
				t.Fatalf("discover calls = %d, want 1", len(reqs))
			}
			cfg, _ := reqs[0]["config"].(map[string]interface{})
			if cfg["database"] != "analytics" || cfg["schema"] != "PUBLIC" {
				t.Errorf("config = %v, want it unchanged", cfg)
			}
			if reqs[0]["include_table_details"] != true {
				t.Errorf("include_table_details = %v, want true", reqs[0]["include_table_details"])
			}
		})
	}
}

// Control: the document path's request is unchanged. MongoDB counts rows with a
// full scan per collection, so it still asks for none, and it never asks for
// table details.
func TestBuildSchemaIndex_DocumentRequestIsUnchanged(t *testing.T) {
	enc := encryptedWarehouseConfig(t, `{"connection_string":"mongodb://h/shop","database":"shop"}`)
	sent := warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) {
		return http.StatusOK, `{"tables":[{"name":"orders","schema":"shop","columns":[{"name":"total","type":"double"}]}]}`
	})

	idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", "mongodb", enc, false)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	if c := idx.Tables[0].Columns[0]; c.Type != "double" {
		t.Errorf("mongodb column type = %q, want the connector's sampled type, double", c.Type)
	}
	req := sent()[0]
	if req["include_row_counts"] != false || req["include_columns"] != true ||
		req["max_tables"] != float64(documentSchemaMaxCollections) {
		t.Errorf("mongodb request changed: %v", req)
	}
	if _, has := req["include_table_details"]; has {
		t.Errorf("mongodb request asks for table details: %v", req)
	}
}

// Control: a connector with no schema to discover (a REST or GraphQL API) still
// gets the empty "unsupported" index, without a discovery call.
func TestBuildSchemaIndex_APIConnectorStaysUnsupported(t *testing.T) {
	enc := encryptedWarehouseConfig(t, `{"token":"x"}`)
	sent := warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) {
		return http.StatusOK, `{"tables":[{"name":"issues"}]}`
	})

	idx, err := buildSchemaIndex(t.Context(), warehouseConnID, "", "github", enc, false)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	if idx.SchemaHash != "unsupported" || idx.TableCount != 0 {
		t.Errorf("index = %+v, want the empty unsupported index", idx)
	}
	if n := len(sent()); n != 0 {
		t.Errorf("discover calls = %d, want 0", n)
	}
}
