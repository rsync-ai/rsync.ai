package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A cache hit on /connections/:id/metadata must answer the same query a fresh
// discovery does. The cache holds every table, and the hit path served it as-is:
// the AI suggestions dialog asked for ?tables=public.subscriptions and got
// users.email too (prod a6ca2103, 2026-10-01). The class is "a cache hit skips the
// query", so every parameter is checked, not only ?tables=.

func cachedSchema(t *testing.T) []byte {
	t.Helper()
	col := func(name string) ColumnMetadata { return ColumnMetadata{Name: name, Type: "text"} }
	b, err := json.Marshal(map[string]interface{}{
		"connection_id":          "c-1",
		"connector_type":         "postgresql",
		"total_tables_available": 4,
		"discovered_at":          "2026-10-01T05:30:00Z",
		"tables": []TableMetadata{
			{Name: "users", Schema: "public", Columns: []ColumnMetadata{col("id"), col("email")}, RowCount: 5005},
			{Name: "subscriptions", Schema: "public", Columns: []ColumnMetadata{col("id"), col("plan")}, RowCount: 1205},
			{Name: "matches", Schema: "public", Columns: []ColumnMetadata{col("id")}, RowCount: 4005},
			{Name: "user_events", Schema: "audit", Columns: []ColumnMetadata{col("id")}, RowCount: -1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type metadataBody struct {
	ConnectionID  string          `json:"connection_id"`
	ConnectorType string          `json:"connector_type"`
	DiscoveredAt  string          `json:"discovered_at"`
	TablesAvail   int             `json:"total_tables_available"`
	Tables        []TableMetadata `json:"tables"`
	Total         int             `json:"total"`
	Limit         int             `json:"limit"`
	Offset        int             `json:"offset"`
}

func queryFrom(t *testing.T, rawQuery string) metadataQuery {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/connections/c-1/metadata?"+rawQuery, nil)
	return parseMetadataQuery(c)
}

func serveCached(t *testing.T, rawQuery string) metadataBody {
	t.Helper()
	body, ok := cachedMetadataResponse(cachedSchema(t), queryFrom(t, rawQuery))
	if !ok {
		t.Fatalf("cached payload not served for %q", rawQuery)
	}
	var got metadataBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func qualifiedTableNames(ts []TableMetadata) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Schema+"."+t.Name)
	}
	return out
}

func TestCachedMetadataResponse_AnswersTheQuery(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantNames []string
		wantTotal int
	}{
		{"named tables only (the suggestions dialog)", "tables=public.subscriptions&include_columns=true",
			[]string{"public.subscriptions"}, 1},
		{"bare and upper-case names match", "tables=SUBSCRIPTIONS,%20matches",
			[]string{"public.matches", "public.subscriptions"}, 2},
		{"search", "search=user&limit=5000", []string{"audit.user_events", "public.users"}, 2},
		{"page", "limit=2&offset=1", []string{"public.subscriptions", "audit.user_events"}, 4},
		{"offset past the end", "limit=2&offset=99", []string{}, 4},
		{"sort by size", "sort_by=size&limit=2", []string{"public.users", "public.matches"}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serveCached(t, tc.query)
			names := qualifiedTableNames(got.Tables)
			if len(names) != len(tc.wantNames) {
				t.Fatalf("tables = %v, want %v", names, tc.wantNames)
			}
			for i := range names {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("tables = %v, want %v", names, tc.wantNames)
				}
			}
			if got.Total != tc.wantTotal {
				t.Fatalf("total = %d, want %d", got.Total, tc.wantTotal)
			}
		})
	}
}

func TestCachedMetadataResponse_ColumnsCountsAndEnvelope(t *testing.T) {
	got := serveCached(t, "include_columns=false&limit=5000")
	for _, tb := range got.Tables {
		if len(tb.Columns) != 0 {
			t.Fatalf("%s.%s kept %d columns with include_columns=false", tb.Schema, tb.Name, len(tb.Columns))
		}
		if tb.RowCount < 0 {
			t.Fatalf("%s.%s row_count = %d, want no negative count", tb.Schema, tb.Name, tb.RowCount)
		}
	}
	if got.ConnectionID != "c-1" || got.ConnectorType != "postgresql" || got.DiscoveredAt == "" || got.TablesAvail != 4 {
		t.Fatalf("envelope lost: %+v", got)
	}
	if got.Limit != 5000 || got.Offset != 0 || got.Total != 4 {
		t.Fatalf("limit/offset/total = %d/%d/%d, want 5000/0/4", got.Limit, got.Offset, got.Total)
	}
	withCols := serveCached(t, "tables=public.users")
	if len(withCols.Tables) != 1 || len(withCols.Tables[0].Columns) != 2 {
		t.Fatalf("include_columns defaults to true: got %+v", withCols.Tables)
	}
}

// A payload the hit path cannot read is not served unfiltered: the caller
// rediscovers instead.
func TestCachedMetadataResponse_UnreadablePayloadIsAMiss(t *testing.T) {
	for _, cached := range []string{`not json`, `{"tables":[{"name":"users","row_count":"12"}]}`} {
		if _, ok := cachedMetadataResponse([]byte(cached), queryFrom(t, "tables=users")); ok {
			t.Fatalf("payload %s was served", cached)
		}
	}
}
