package handlers

import (
	"errors"
	"testing"

	"github.com/rsync-ai/shared/sqlexec"
)

// engineKind is how sqlexec.For runs a connector type's statements: "direct" (a
// driver or the Databricks API), "delegated" (the connector, through the
// orchestrator), or "" when it has no engine.
func engineKind(t *testing.T, connectorType string) string {
	t.Helper()
	eng, err := explorerEngine(connectorType, nil)
	switch {
	case errors.Is(err, sqlexec.ErrUnsupported):
		return ""
	case err != nil:
		t.Fatalf("%q: For: %v", connectorType, err)
	case sqlexec.IsDelegated(eng):
		return "delegated"
	}
	return "direct"
}

// TestExplorerEngineMatchesCapability keeps the one dispatcher (sqlexec.For) and the
// capability table from drifting: a connection the Explorer lists has an engine, one
// it does not list has none, and an engine is delegated exactly when the connection's
// schema comes from its connector.
func TestExplorerEngineMatchesCapability(t *testing.T) {
	for _, ct := range []string{
		"postgresql", "postgres", "azure-postgres", "supabase-postgres", "redshift",
		"mysql", "mariadb", "azure-mysql", "sqlserver", "mssql", "azure-sqlserver",
		"databricks", "bigquery", "clickhouse", "snowflake", "oracle",
		"mongodb", "mongodb-atlas", "MongoDB", "PostgreSQL", "MySQL", "BigQuery",
		"aws-s3", "gcs", "azure-blob", "minio", "shopify", "stripe", "github",
		"metabase", "kafka", "", "unknown-connector",
	} {
		t.Run(ct, func(t *testing.T) {
			cap := ResolveExplorerCapability(ct)
			kind := engineKind(t, ct)
			if (kind != "") != cap.Supported {
				t.Fatalf("%q: engine %q but Supported=%v", ct, kind, cap.Supported)
			}
			if !cap.Supported {
				return
			}
			if wantDelegated := cap.SchemaStrategy == schemaMCPDiscover; (kind == "delegated") != wantDelegated {
				t.Errorf("%q: engine %q but SchemaStrategy=%q", ct, kind, cap.SchemaStrategy)
			}
		})
	}
}

// TestEveryDialectMapAgrees is the bug class behind BigQuery SQL written as
// PostgreSQL: one connector type, three maps — the capability table (what the
// connections API and the query gates say), SQLDialect (what the
// SQL writer is told) and modelDialect (how a model's DDL is written). The
// writer's map had no BigQuery case, so a BigQuery connection fell through to
// the "postgresql" default. Every map must name the same engine for every type.
func TestEveryDialectMapAgrees(t *testing.T) {
	// modelDialect names two engines its own way.
	modelLabel := map[string]string{"postgresql": "postgres", "tsql": "sqlserver"}
	for _, ct := range []string{
		"postgresql", "postgres", "azure-postgres", "supabase-postgres", "redshift",
		"mysql", "mariadb", "azure-mysql", "mysql-cdc", "sqlserver", "mssql", "azure-sqlserver",
		"databricks", "bigquery", "BigQuery", "clickhouse", "snowflake", "oracle",
		"mongodb", "mongodb-atlas", "MongoDB", "PostgreSQL", "MySQL",
		"aws-s3", "gcs", "azure-blob", "minio", "shopify", "stripe", "github",
		"google-sheets", "metabase", "kafka", "", "unknown-connector",
	} {
		t.Run(ct, func(t *testing.T) {
			cap := ResolveExplorerCapability(ct)
			want := ""
			if cap.QueryLanguage == langSQL {
				want = cap.Dialect
				if want == "" {
					t.Fatalf("%q: a SQL connection with no dialect", ct)
				}
			}
			if got := SQLDialect(ct); got != want {
				t.Errorf("%q: the SQL writer is told %q, the capability table says %q", ct, got, want)
			}
			wantModel := want
			if l, ok := modelLabel[want]; ok {
				wantModel = l
			}
			if got := modelDialect(ct); got != wantModel {
				t.Errorf("%q: modelDialect=%q, the capability table says %q (%q)", ct, got, want, wantModel)
			}
		})
	}
}

// TestResolveExplorerCapability_DirectDriverParity pins the behavior-preserving
// contract of the resolver refactor: every connector_type that the legacy inline
// gates (the per-handler isPostgres/isMySQL/isDatabricks/isSQLServer blocks)
// accepted MUST still resolve to a direct engine with the same dialect. If this
// test fails, the resolver dropped or re-routed a previously-working engine.
func TestResolveExplorerCapability_DirectDriverParity(t *testing.T) {
	cases := []struct {
		connectorType string
		wantDialect   string
	}{
		{"postgresql", "postgresql"},
		{"postgres", "postgresql"},
		{"azure-postgres", "postgresql"},
		{"redshift", "postgresql"}, // Redshift reuses the Postgres driver
		{"mysql", "mysql"},
		{"mariadb", "mysql"},
		{"sqlserver", "tsql"},
		{"mssql", "tsql"},
		{"databricks", "databricks"},
	}
	for _, tc := range cases {
		t.Run(tc.connectorType, func(t *testing.T) {
			cap := ResolveExplorerCapability(tc.connectorType)
			if !cap.Supported {
				t.Fatalf("%q: Supported=false, want true (direct-driver engine dropped)", tc.connectorType)
			}
			if kind := engineKind(t, tc.connectorType); kind != "direct" {
				t.Errorf("%q: engine %q, want direct", tc.connectorType, kind)
			}
			if cap.QueryLanguage != langSQL {
				t.Errorf("%q: QueryLanguage=%q, want %q", tc.connectorType, cap.QueryLanguage, langSQL)
			}
			if cap.SchemaStrategy != schemaSQLIntrospection {
				t.Errorf("%q: SchemaStrategy=%q, want %q", tc.connectorType, cap.SchemaStrategy, schemaSQLIntrospection)
			}
			if cap.Dialect != tc.wantDialect {
				t.Errorf("%q: Dialect=%q, want %q", tc.connectorType, cap.Dialect, tc.wantDialect)
			}
		})
	}
}

// TestResolveExplorerCapability_Delegated covers the newly-added delegated engine
// (BigQuery — a data warehouse whose SQL is executed through the connector's MCP
// export tool rather than an in-gateway driver).
func TestResolveExplorerCapability_Delegated(t *testing.T) {
	t.Run("bigquery", func(t *testing.T) {
		cap := ResolveExplorerCapability("bigquery")
		if !cap.Supported || engineKind(t, "bigquery") != "delegated" || cap.QueryLanguage != langSQL {
			t.Fatalf("bigquery: %+v, want supported delegated SQL", cap)
		}
		if cap.SchemaStrategy != schemaMCPDiscover {
			t.Errorf("bigquery: SchemaStrategy=%q, want %q", cap.SchemaStrategy, schemaMCPDiscover)
		}
		if cap.Dialect != "bigquery" {
			t.Errorf("bigquery: Dialect=%q, want bigquery", cap.Dialect)
		}
	})
	t.Run("clickhouse", func(t *testing.T) {
		cap := ResolveExplorerCapability("clickhouse")
		if !cap.Supported || engineKind(t, "clickhouse") != "delegated" || cap.QueryLanguage != langSQL {
			t.Fatalf("clickhouse: %+v, want supported delegated SQL", cap)
		}
		if cap.SchemaStrategy != schemaMCPDiscover {
			t.Errorf("clickhouse: SchemaStrategy=%q, want %q", cap.SchemaStrategy, schemaMCPDiscover)
		}
		if cap.Dialect != "clickhouse" {
			t.Errorf("clickhouse: Dialect=%q, want clickhouse", cap.Dialect)
		}
	})
	// Snowflake and Oracle: the gateway has no driver for either, so both delegate
	// to the connector's MCP tools like BigQuery.
	for _, ct := range []string{"snowflake", "oracle"} {
		t.Run(ct, func(t *testing.T) {
			cap := ResolveExplorerCapability(ct)
			if !cap.Supported || engineKind(t, ct) != "delegated" || cap.QueryLanguage != langSQL {
				t.Fatalf("%s: %+v, want supported delegated SQL", ct, cap)
			}
			if cap.SchemaStrategy != schemaMCPDiscover {
				t.Errorf("%s: SchemaStrategy=%q, want %q", ct, cap.SchemaStrategy, schemaMCPDiscover)
			}
			if cap.Dialect != ct {
				t.Errorf("%s: Dialect=%q, want %s", ct, cap.Dialect, ct)
			}
			// NL->SQL must generate in the engine's own dialect, not the postgresql default.
			if d := SQLDialect(ct); d != cap.Dialect {
				t.Errorf("%s: dialect drift — resolver=%q SQLDialect=%q", ct, cap.Dialect, d)
			}
		})
	}
}

// TestResolveExplorerCapability_Unsupported ensures non-queryable connectors — SaaS
// and object-store connectors the Explorer must never surface (they 400 on query) —
// resolve to Supported=false so the frontend filter and the gateway gate both exclude
// them without any defensive code.
func TestResolveExplorerCapability_Unsupported(t *testing.T) {
	for _, ct := range []string{"shopify", "aws-s3", "gcs", "azure-blob", "stripe", "github", "metabase", "", "unknown-connector"} {
		if cap := ResolveExplorerCapability(ct); cap.Supported {
			t.Errorf("%q: Supported=true, want false (must not appear in Explorer)", ct)
		}
	}
}

// TestResolveExplorerCapability_MongoDocumentMode pins MongoDB to document mode:
// surfaced, but never with a SQL dialect or query language, so none of the SQL paths
// (query, export, NL->SQL) can accept it. It materializes — a model is a pipeline
// whose result the connector writes with $out.
func TestResolveExplorerCapability_MongoDocumentMode(t *testing.T) {
	for _, ct := range []string{"mongodb", "MongoDB", "mongodb-atlas"} {
		cap := ResolveExplorerCapability(ct)
		want := ExplorerCapability{
			QueryLanguage: langDocument, SchemaStrategy: schemaMCPDiscover,
			Supported: true, SupportsMaterialization: true,
		}
		if cap != want {
			t.Errorf("%q: got %+v, want %+v", ct, cap, want)
		}
		if kind := engineKind(t, ct); kind != "delegated" {
			t.Errorf("%q: engine %q, want delegated (the connector's find tool)", ct, kind)
		}
		if d := SQLDialect(ct); d != "" {
			t.Errorf("%q: SQLDialect=%q, want empty (no NL->SQL for documents)", ct, d)
		}
	}
}

// TestResolveExplorerCapability_CaseInsensitive guards the substring/lowercasing
// contract the legacy gates relied on.
func TestResolveExplorerCapability_CaseInsensitive(t *testing.T) {
	for _, ct := range []string{"PostgreSQL", "MySQL", "SQLServer", "Databricks", "BigQuery", "Snowflake", "Oracle"} {
		if cap := ResolveExplorerCapability(ct); !cap.Supported {
			t.Errorf("%q: Supported=false, want true (case-insensitive match broken)", ct)
		}
	}
}

// TestSupportsMaterializationMatchesModelDialect keeps the capability table and the
// model path from drifting apart. SupportsMaterialization is what the UI reads to
// decide whether to offer the control; modelDialect() is what actually builds the
// rebuild plan. If someone adds a warehouse to one and forgets the other, the two
// disagree in the worst possible direction: the UI offers materialization and the
// 03:00 tick refuses it.
//
// Same shape as the dialect-drift assertion in _DirectDriverParity above.
func TestSupportsMaterializationMatchesModelDialect(t *testing.T) {
	// Every connector the resolver has a case for, plus the unsupported tail.
	for _, ct := range []string{
		"postgresql", "postgres", "azure-postgres", "redshift",
		"mysql", "mariadb", "sqlserver", "mssql",
		"databricks", "bigquery", "clickhouse", "snowflake", "oracle",
		"mongodb", "aws-s3", "stripe", "", "unknown-connector",
	} {
		t.Run(ct, func(t *testing.T) {
			cap := ResolveExplorerCapability(ct)
			wantMaterializable := modelSupported(ct)
			if cap.SupportsMaterialization != wantMaterializable {
				t.Errorf("%q: SupportsMaterialization=%v but modelDialect=%q, document=%v — capability table and model path have drifted",
					ct, cap.SupportsMaterialization, modelDialect(ct), isDocumentConnector(ct))
			}
			// A connection that cannot be queried cannot be a model either. The
			// converse is deliberately false: queryable is not materializable.
			if cap.SupportsMaterialization && !cap.Supported {
				t.Errorf("%q: SupportsMaterialization=true with Supported=false", ct)
			}
		})
	}
}

// TestSupportsMaterializationCoversEveryWarehouse pins the engines a model runs on
// without a gateway driver (saved_query_models_engines.go), and MongoDB, whose model
// is a pipeline rather than SQL (saved_query_documents.go).
func TestSupportsMaterializationCoversEveryWarehouse(t *testing.T) {
	for _, ct := range []string{"bigquery", "clickhouse", "databricks", "snowflake", "oracle"} {
		cap := ResolveExplorerCapability(ct)
		if !cap.Supported {
			t.Fatalf("%q: Supported=false — a model's SQL is written in the Explorer", ct)
		}
		if !cap.SupportsMaterialization {
			t.Errorf("%q: SupportsMaterialization=false, want true (models run on every SQL engine)", ct)
		}
	}
	if cap := ResolveExplorerCapability("mongodb"); !cap.SupportsMaterialization || cap.QueryLanguage != langDocument {
		t.Errorf("mongodb: SupportsMaterialization=%v QueryLanguage=%q, want a materializable document connection",
			cap.SupportsMaterialization, cap.QueryLanguage)
	}
}
