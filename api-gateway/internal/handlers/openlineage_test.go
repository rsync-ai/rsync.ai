package handlers

// The OpenLineage events (design § 5): how each store's tables are named, that the
// naming reads no credential, the RunEvent and its facets, and that nothing but names,
// the column mapping, times and an outcome leaves.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenLineageStoreFor_NamesEachStore(t *testing.T) {
	for _, tc := range []struct {
		label, typ string
		cfg        map[string]any
		namespace  string
		names      map[string]string // the run's name → the dataset name
	}{
		{"postgres, port a number, no schema", "postgresql",
			map[string]any{"host": "DB.Example.com", "port": float64(6543), "database": "Shop"},
			"postgres://db.example.com:6543",
			map[string]string{"orders": "shop.public.orders", "analytics.orders": "shop.analytics.orders", "a.b.c": "a.b.c"}},
		{"postgres, a schema set", "postgres",
			map[string]any{"host": "db", "dbname": "shop", "schema": "Sales"},
			"postgres://db:5432",
			map[string]string{"orders": "shop.sales.orders", "analytics.orders": "shop.analytics.orders"}},
		{"postgres family, an IPv6 host", "aurora-postgresql",
			map[string]any{"host": "::1", "database": "shop"},
			"postgres://[::1]:5432", nil},
		{"redshift cluster endpoint", "redshift",
			map[string]any{"host": "my-cluster.abc123xyz.us-east-1.redshift.amazonaws.com", "database": "dev"},
			"redshift://my-cluster.us-east-1:5439",
			map[string]string{"orders": "dev.public.orders"}},
		{"redshift other host", "redshift",
			map[string]any{"host": "proxy.internal", "port": "5440", "database": "dev"},
			"redshift://proxy.internal:5440", nil},
		{"mysql, port a string", "mariadb",
			map[string]any{"hostname": "my", "port": "3307", "database": "shop"},
			"mysql://my:3307",
			map[string]string{"orders": "shop.orders", "other.orders": "other.orders"}},
		{"sql server", "sql-server",
			map[string]any{"server": "ms", "database": "Shop"},
			"mssql://ms:1433",
			map[string]string{"orders": "shop.dbo.orders"}},
		{"oracle by service name", "oracle",
			map[string]any{"host": "ora", "service_name": "ORCLPDB1", "database": "ignored"},
			"oracle://ora:1521",
			map[string]string{"hr.employees": "orclpdb1.hr.employees"}},
		{"oracle by sid", "oracle",
			map[string]any{"host": "ora", "sid": "XE"},
			"oracle://ora:1521",
			map[string]string{"hr.employees": "xe.hr.employees"}},
		{"mongodb", "mongo",
			map[string]any{"host": "mongo", "database": "app"},
			"mongodb://mongo:27017",
			map[string]string{"users": "app.users"}},
		{"bigquery", "bigquery",
			map[string]any{"project_id": "Proj", "dataset_id": "raw"},
			"bigquery",
			map[string]string{"orders": "proj.raw.orders", "marts.orders": "proj.marts.orders"}},
		{"bigquery, no dataset", "bq",
			map[string]any{"project": "proj"},
			"bigquery",
			map[string]string{"orders": "proj.orders"}},
		{"snowflake", "snowflake",
			map[string]any{"account": "XY12345.us-east-1", "database": "ANALYTICS"},
			"snowflake://xy12345.us-east-1",
			map[string]string{"orders": "analytics.public.orders"}},
		{"snowflake with no account", "snowflake",
			map[string]any{"database": "ANALYTICS"},
			openLineageRsyncNamespace + leWH, map[string]string{"orders": "orders"}},
		{"postgres with no host", "postgresql",
			map[string]any{"database": "shop"},
			openLineageRsyncNamespace + leWH, map[string]string{"orders": "orders"}},
		{"an object store", "s3",
			map[string]any{"bucket": "lake", "host": "s3.amazonaws.com"},
			openLineageRsyncNamespace + leWH, map[string]string{"events/2026": "events/2026"}},
		{"no settings", "postgresql", nil, openLineageRsyncNamespace + leWH, nil},
	} {
		s := openLineageStoreFor(leWH, tc.typ, tc.cfg)
		if s.Namespace != tc.namespace {
			t.Errorf("%s: namespace %q, want %q", tc.label, s.Namespace, tc.namespace)
		}
		for in, want := range tc.names {
			if got := s.name(in); got != want {
				t.Errorf("%s: name(%q) = %q, want %q", tc.label, in, got, want)
			}
		}
	}
}

// A part the settings do not give is left out, not written empty.
func TestOpenLineageStoreName_UnknownLeadingParts(t *testing.T) {
	s := openLineageStore{Namespace: "postgres://db:5432", Parts: 3, Defaults: []string{"", "public"}}
	if got := s.name("orders"); got != "public.orders" {
		t.Errorf("name = %q", got)
	}
	if got := s.name(""); got != "" {
		t.Errorf("an empty name = %q", got)
	}
}

// The naming reads only openLineageConfigKeys. A connection whose host is only in its
// connection string gets the rsync.ai namespace, and no credential reaches an event.
func TestOpenLineageStoreFor_ReadsNoCredential(t *testing.T) {
	const canary = "CANARY-7f3a"
	cfg := map[string]any{
		"connection_string": "postgres://admin:" + canary + "@db.example.com:5432/shop",
		"uri":               "postgres://admin:" + canary + "@db.example.com/shop",
		"dsn":               canary,
		"password":          canary,
		"user":              canary,
		"username":          canary,
		"api_key":           canary,
		"token":             canary,
		"private_key":       canary,
		"credentials_json":  `{"private_key":"` + canary + `"}`,
		"ssl_key":           canary,
		"service_account":   canary,
	}
	for _, typ := range []string{"postgresql", "redshift", "mysql", "sqlserver", "oracle", "mongodb", "bigquery", "snowflake", "s3"} {
		s := openLineageStoreFor(leWH, typ, cfg)
		if typ != "bigquery" && s.Namespace != openLineageRsyncNamespace+leWH {
			t.Errorf("%s: a host from a connection string: %q", typ, s.Namespace)
		}
		ev := buildOpenLineageEvent(openLineageEventRow{
			ID: "ev-1", WorkspaceID: leWS, JobKind: lineageJobModel, JobID: leModel, JobName: "m", RunRef: "r",
			EventType: lineageEventComplete, EndedAt: leT0, ModelConnectionID: leWH,
			Inputs:  []lineageDataset{{ConnectionID: leWH, Name: "orders"}},
			Outputs: []lineageDataset{{ConnectionID: leWH, Name: "marts.orders"}},
			Columns: &savedColumnLineage{Columns: []string{"id"}, Edges: []columnLineageEdge{
				{Table: []string{"orders"}, Column: "id", ToColumn: "id", Type: "DIRECT", Subtype: "IDENTITY"}}},
		}, map[string]openLineageStore{leWH: s})
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), canary) || strings.Contains(string(b), "admin") {
			t.Errorf("%s: a credential reached the event: %s", typ, b)
		}
	}

	// With a host given, the credentials beside it stay out as well.
	cfg["host"], cfg["database"] = "db.example.com", "shop"
	s := openLineageStoreFor(leWH, "postgresql", cfg)
	if s.Namespace != "postgres://db.example.com:5432" {
		t.Fatalf("namespace %q", s.Namespace)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), canary) {
		t.Errorf("the store holds a credential: %s", b)
	}
}

// A model run: its event, its job and its output's schema and column lineage.
func TestBuildOpenLineageEvent_ModelRunWithColumns(t *testing.T) {
	started := leT0
	stores := map[string]openLineageStore{
		leWH: openLineageStoreFor(leWH, "postgresql", map[string]any{"host": "db", "database": "shop"}),
	}
	row := openLineageEventRow{
		ID: "e0000000-0000-4000-8000-0000000000e1", WorkspaceID: leWS, JobKind: lineageJobModel, JobID: leModel,
		JobName: "orders_enriched", RunRef: "run-1", EventType: lineageEventComplete,
		StartedAt: &started, EndedAt: leT0.Add(90 * time.Second), ModelConnectionID: leWH,
		Inputs:  []lineageDataset{{ConnectionID: leWH, Name: "analytics.orders"}, {ConnectionID: leWH, Name: "customers"}},
		Outputs: []lineageDataset{{ConnectionID: leWH, Name: "marts.orders_enriched"}},
		Columns: &savedColumnLineage{
			Columns: []string{"id", "name", "total"},
			Edges: []columnLineageEdge{
				{Table: []string{"analytics", "orders"}, Column: "id", ToColumn: "id", Type: "DIRECT", Subtype: "IDENTITY"},
				{Table: []string{"Customers"}, Column: "name", ToColumn: "name", Type: "DIRECT", Subtype: "IDENTITY"},
				{Table: []string{"analytics", "orders"}, Column: "amount", ToColumn: "total", Type: "DIRECT", Subtype: "AGGREGATION"},
				{Table: []string{"analytics", "orders"}, Column: "status", Type: "INDIRECT", Subtype: "FILTER"},
				{Table: nil, Column: "x", ToColumn: "id", Type: "DIRECT"},             // no table: left out
				{Table: []string{"analytics", "orders"}, Column: "y", Type: "DIRECT"}, // DIRECT to no column: left out
			},
		},
	}
	b, err := json.Marshal(buildOpenLineageEvent(row, stores))
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	get := func(path ...any) any {
		var cur any = ev
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, _ := cur.(map[string]any)
				cur = m[k]
			case int:
				a, _ := cur.([]any)
				if k >= len(a) {
					return nil
				}
				cur = a[k]
			}
		}
		return cur
	}
	for _, c := range []struct {
		path []any
		want any
	}{
		{[]any{"eventType"}, "COMPLETE"},
		{[]any{"eventTime"}, "2026-10-14T09:01:30Z"},
		{[]any{"producer"}, "https://rsync.ai"},
		{[]any{"schemaURL"}, "https://openlineage.io/spec/2-0-2/OpenLineage.json#/$defs/RunEvent"},
		{[]any{"run", "runId"}, "e0000000-0000-4000-8000-0000000000e1"},
		{[]any{"run", "facets", "nominalTime", "nominalStartTime"}, "2026-10-14T09:00:00Z"},
		{[]any{"run", "facets", "nominalTime", "nominalEndTime"}, "2026-10-14T09:01:30Z"},
		{[]any{"run", "facets", "nominalTime", "_producer"}, "https://rsync.ai"},
		{[]any{"run", "facets", "nominalTime", "_schemaURL"}, openLineageNominalTimeURL},
		{[]any{"job", "namespace"}, "rsync.ai/" + leWS},
		{[]any{"job", "name"}, "orders_enriched (model " + leModel + ")"},
		{[]any{"job", "facets", "jobType", "processingType"}, "BATCH"},
		{[]any{"job", "facets", "jobType", "integration"}, "RSYNC_AI"},
		{[]any{"job", "facets", "jobType", "jobType"}, "MODEL"},
		{[]any{"job", "facets", "jobType", "_schemaURL"}, openLineageJobTypeURL},
		{[]any{"inputs", 0, "namespace"}, "postgres://db:5432"},
		{[]any{"inputs", 0, "name"}, "shop.analytics.orders"},
		{[]any{"inputs", 1, "name"}, "shop.public.customers"},
		{[]any{"outputs", 0, "name"}, "shop.marts.orders_enriched"},
		{[]any{"outputs", 0, "facets", "schema", "_schemaURL"}, openLineageSchemaFacetURL},
		{[]any{"outputs", 0, "facets", "schema", "fields", 2, "name"}, "total"},
		{[]any{"outputs", 0, "facets", "columnLineage", "_schemaURL"}, openLineageColumnFacetURL},
		{[]any{"outputs", 0, "facets", "columnLineage", "fields", "id", "inputFields", 0, "name"}, "shop.analytics.orders"},
		{[]any{"outputs", 0, "facets", "columnLineage", "fields", "id", "inputFields", 0, "field"}, "id"},
		{[]any{"outputs", 0, "facets", "columnLineage", "fields", "name", "inputFields", 0, "name"}, "shop.public.customers"},
		{[]any{"outputs", 0, "facets", "columnLineage", "fields", "total", "inputFields", 0, "field"}, "amount"},
		{[]any{"outputs", 0, "facets", "columnLineage", "fields", "total", "inputFields", 0, "transformations", 0, "subtype"}, "AGGREGATION"},
		{[]any{"outputs", 0, "facets", "columnLineage", "dataset", 0, "field"}, "status"},
		{[]any{"outputs", 0, "facets", "columnLineage", "dataset", 0, "transformations", 0, "type"}, "INDIRECT"},
		{[]any{"outputs", 0, "facets", "columnLineage", "dataset", 1}, nil},
	} {
		if got := get(c.path...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v = %#v, want %#v", c.path, got, c.want)
		}
	}
	if fields, _ := get("outputs", 0, "facets", "schema", "fields", 0).(map[string]any); len(fields) != 1 {
		t.Errorf("a schema field carries more than its name: %v", fields)
	}
	if n := len(get("outputs", 0, "facets", "columnLineage", "fields").(map[string]any)); n != 3 {
		t.Errorf("%d column lineage fields, want 3", n)
	}
	if get("inputs", 0, "facets") != nil {
		t.Errorf("an input carries facets")
	}
}

// A CDC window is a streaming pipeline job; a table it captured under no destination
// name keeps the rsync.ai namespace and its source-side name.
func TestBuildOpenLineageEvent_CDCWindowAndCapturedOutput(t *testing.T) {
	stores := map[string]openLineageStore{
		leSource: openLineageStoreFor(leSource, "mysql", map[string]any{"host": "src", "database": "shop"}),
		leWH:     openLineageStoreFor(leWH, "bigquery", map[string]any{"project_id": "p", "dataset_id": "raw"}),
	}
	row := openLineageEventRow{
		ID: "ev-2", WorkspaceID: leWS, JobKind: lineageJobPipeline, JobID: lePipeline, JobName: "",
		RunRef: cdcTrailingEdgeOccurrence(15*time.Minute, leT0), EventType: lineageEventComplete, EndedAt: leT0,
		Inputs:  []lineageDataset{{ConnectionID: leSource, Name: "orders"}},
		Outputs: []lineageDataset{{ConnectionID: leWH, Name: "orders"}, {ConnectionID: leWH, Name: "events", Captured: true}},
		// A pipeline's event never carries a model's facets.
		Columns: &savedColumnLineage{Columns: []string{"id"}},
	}
	ev := buildOpenLineageEvent(row, stores)
	if ev.Job.Facets.JobType.ProcessingType != "STREAMING" || ev.Job.Facets.JobType.JobType != "PIPELINE" {
		t.Errorf("job type = %+v", ev.Job.Facets.JobType)
	}
	if ev.Job.Name != "pipeline "+lePipeline {
		t.Errorf("an unnamed job = %q", ev.Job.Name)
	}
	if ev.Run.Facets.NominalTime != nil {
		t.Errorf("a run with no start has a nominal time")
	}
	want := []openLineageInput{
		{Namespace: "bigquery", Name: "p.raw.orders"},
		{Namespace: openLineageRsyncNamespace + leWH, Name: "events"},
	}
	if !reflect.DeepEqual(ev.Outputs, want) {
		t.Errorf("outputs = %+v, want %+v", ev.Outputs, want)
	}
	if !reflect.DeepEqual(ev.Inputs, []openLineageInput{{Namespace: "mysql://src:3306", Name: "shop.orders"}}) {
		t.Errorf("inputs = %+v", ev.Inputs)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), `"run":{"runId":"ev-2","facets":{}}`) {
		t.Errorf("run = %s", b)
	}

	// A batch run is BATCH, and a connection with no naming is named by its id.
	row.RunRef, row.Outputs = "e0000000-0000-4000-8000-0000000000f1", []lineageDataset{{ConnectionID: leModel, Name: "x"}}
	ev = buildOpenLineageEvent(row, stores)
	if ev.Job.Facets.JobType.ProcessingType != "BATCH" || ev.Outputs[0].Namespace != openLineageRsyncNamespace+leModel {
		t.Errorf("batch run = %+v %+v", ev.Job.Facets.JobType, ev.Outputs)
	}
}

// From a model whose SQL holds a literal to the JSON that leaves: names, the mapping,
// times and the outcome, never the SQL, a literal or a count.
func TestOpenLineageEvent_CarriesNoSQLOrValues(t *testing.T) {
	const literal = "SECRET-LITERAL"
	r := leModelRun() // its SQL filters on 'SECRET-LITERAL'
	if !strings.Contains(r.SQLText, literal) {
		t.Fatal("control: the run's SQL holds the marker")
	}
	le := modelRunEvent(r)
	row := openLineageEventRow{
		ID: "ev-3", WorkspaceID: le.WorkspaceID, JobKind: le.JobKind, JobID: le.SavedQueryID, JobName: r.Name,
		RunRef: le.RunRef, EventType: le.EventType, StartedAt: &le.StartedAt.Time, EndedAt: le.EndedAt,
		Inputs: le.Inputs, Outputs: le.Outputs, ModelConnectionID: r.ConnectionID,
		Columns: &savedColumnLineage{Columns: []string{"id", "name"}, Truncated: true, Edges: []columnLineageEdge{
			{Table: []string{"analytics", "orders"}, Column: "note", Type: "INDIRECT", Subtype: "FILTER"}}},
	}
	b, err := json.Marshal(buildOpenLineageEvent(row, map[string]openLineageStore{
		leWH: openLineageStoreFor(leWH, r.ConnectorType, map[string]any{"host": "db", "database": "shop"}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, banned := range []string{literal, "SELECT", "JOIN", "WHERE", `"sql"`, `"query"`, "outputStatistics",
		"rowCount", "dataQualityMetrics", "errorMessage", "inputStatistics", r.SQLText} {
		if strings.Contains(out, banned) {
			t.Errorf("the event carries %q: %s", banned, out)
		}
	}
	if !strings.Contains(out, `"field":"note"`) || !strings.Contains(out, "shop.analytics.orders") {
		t.Errorf("control: the event names the filter's column and table: %s", out)
	}
}
