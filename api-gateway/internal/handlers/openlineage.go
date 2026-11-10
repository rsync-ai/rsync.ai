package handlers

// OpenLineage events, design step L3 (docs/internal/lineage-catalog-design.md § 5): each
// lineage_events row as one RunEvent of the OpenLineage spec 2-0-2, so a catalog that
// reads OpenLineage (Marquez, DataHub, OpenMetadata, Atlan, a Unity Catalog bridge) gets
// rsync.ai's runs and column lineage.
//
// This file builds the events and is pure; openlineage_export.go reads them, serves the
// pull and runs the push. Both are L3's own files with one registration point
// (RegisterOpenLineage), so the export can move to another edition whole.
//
// What leaves rsync.ai (design § 5.3): job, table and column names, the column mapping,
// run times and how a run ended. Never the SQL, a row count, a row value, a credential or
// an error text: no sql, outputStatistics, dataQualityMetrics or errorMessage facet is
// built, and a test checks a model whose SQL and run carry marker strings.

import (
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	openLineageProducer  = "https://rsync.ai"
	openLineageSchemaURL = "https://openlineage.io/spec/2-0-2/OpenLineage.json#/$defs/RunEvent"

	openLineageNominalTimeURL   = "https://openlineage.io/spec/facets/1-0-1/NominalTimeRunFacet.json#/$defs/NominalTimeRunFacet"
	openLineageJobTypeURL       = "https://openlineage.io/spec/facets/2-0-3/JobTypeJobFacet.json#/$defs/JobTypeJobFacet"
	openLineageSchemaFacetURL   = "https://openlineage.io/spec/facets/1-1-1/SchemaDatasetFacet.json#/$defs/SchemaDatasetFacet"
	openLineageColumnFacetURL   = "https://openlineage.io/spec/facets/1-2-0/ColumnLineageDatasetFacet.json#/$defs/ColumnLineageDatasetFacet"
	openLineageIntegration      = "RSYNC_AI"
	openLineageRsyncNamespace   = "rsync.ai://connection/"
	openLineageJobNamespaceHead = "rsync.ai/"
)

// openLineageStore is how one connection's tables are named (OpenLineage naming): the
// dataset namespace, and the leading name parts a name the run gave fewer of starts
// with. Read from the connection's non-secret settings only (openLineageStoreFor).
type openLineageStore struct {
	Namespace string
	// Parts is how many parts a full name has, 0 to keep a name as the run gave it.
	Parts int
	// Defaults are the parts before the table's, the database first, "" when not known.
	Defaults []string
}

// name is a table's dataset name: the run's name with the leading parts it lacks.
func (s openLineageStore) name(n string) string {
	if s.Parts == 0 || n == "" {
		return n
	}
	parts := strings.Split(n, ".")
	if len(parts) >= s.Parts {
		return n
	}
	// The run's name is the last parts, so the ones it lacks are the first.
	lead := s.Defaults
	if missing := s.Parts - len(parts); len(lead) > missing {
		lead = lead[:missing]
	}
	out := []string{}
	for _, p := range lead {
		if p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return strings.Join(append(out, parts...), ".")
}

// openLineageConnectionStore names a connection rsync.ai cannot say more about: an
// object store, a store OpenLineage has no naming for, or a connection since deleted.
func openLineageConnectionStore(connectionID string) openLineageStore {
	return openLineageStore{Namespace: openLineageRsyncNamespace + connectionID}
}

// openLineageConfigKeys are the only settings the naming reads. Every other key, and
// every credential among them, is never read (design § 5.1).
var openLineageConfigKeys = struct {
	Host, Port, Database, Schema, Project, Dataset, Account, Service, SID []string
}{
	Host:     []string{"host", "hostname", "server"},
	Port:     []string{"port"},
	Database: []string{"database", "db_name", "dbname", "db"},
	Schema:   []string{"schema", "default_schema"},
	Project:  []string{"project_id", "project"},
	Dataset:  []string{"dataset_id", "dataset"},
	Account:  []string{"account", "account_identifier"},
	// Oracle's service as cdc/oracle.go reads it: service_name, service, database, sid.
	Service: []string{"service_name", "service"},
	SID:     []string{"sid"},
}

var redshiftHostRe = regexp.MustCompile(`^([a-z0-9-]+)\.[a-z0-9]+\.([a-z0-9-]+)\.redshift(?:-serverless)?\.amazonaws\.com$`)

// openLineageStoreFor names a connection's tables from its connector type and its
// settings, read through openLineageConfigKeys only. A store whose host is not in its
// settings (only in a connection string, which is a credential) gets the rsync.ai
// namespace.
func openLineageStoreFor(connectionID, connectorType string, cfg map[string]any) openLineageStore {
	str := func(keys []string) string { return configString(cfg, keys...) }
	host := strings.ToLower(str(openLineageConfigKeys.Host))
	port := openLineagePort(cfg)
	hostPort := func(defaultPort int) string {
		if port == 0 {
			port = defaultPort
		}
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	db := str(openLineageConfigKeys.Database)
	schema := str(openLineageConfigKeys.Schema)
	switch normalizeConnectorFamily(connectorType) {
	case "postgres":
		if host == "" {
			break
		}
		return openLineageStore{Namespace: "postgres://" + hostPort(5432), Parts: 3, Defaults: []string{db, firstNonEmpty(schema, "public")}}
	case "redshift":
		if host == "" {
			break
		}
		if m := redshiftHostRe.FindStringSubmatch(host); m != nil {
			if port == 0 {
				port = 5439
			}
			return openLineageStore{Namespace: "redshift://" + m[1] + "." + m[2] + ":" + strconv.Itoa(port), Parts: 3, Defaults: []string{db, firstNonEmpty(schema, "public")}}
		}
		return openLineageStore{Namespace: "redshift://" + hostPort(5439), Parts: 3, Defaults: []string{db, firstNonEmpty(schema, "public")}}
	case "mysql":
		if host == "" {
			break
		}
		return openLineageStore{Namespace: "mysql://" + hostPort(3306), Parts: 2, Defaults: []string{db}}
	case "mssql":
		if host == "" {
			break
		}
		return openLineageStore{Namespace: "mssql://" + hostPort(1433), Parts: 3, Defaults: []string{db, firstNonEmpty(schema, "dbo")}}
	case "oracle":
		if host == "" {
			break
		}
		service := firstNonEmpty(str(openLineageConfigKeys.Service), db, str(openLineageConfigKeys.SID))
		return openLineageStore{Namespace: "oracle://" + hostPort(1521), Parts: 3, Defaults: []string{service, ""}}
	case "mongodb":
		if host == "" {
			break
		}
		return openLineageStore{Namespace: "mongodb://" + hostPort(27017), Parts: 2, Defaults: []string{db}}
	case "bigquery":
		return openLineageStore{Namespace: "bigquery", Parts: 3,
			Defaults: []string{str(openLineageConfigKeys.Project), str(openLineageConfigKeys.Dataset)}}
	case "snowflake":
		account := strings.ToLower(str(openLineageConfigKeys.Account))
		if account == "" {
			break
		}
		return openLineageStore{Namespace: "snowflake://" + account, Parts: 3,
			Defaults: []string{db, firstNonEmpty(schema, "public")}}
	}
	return openLineageConnectionStore(connectionID)
}

// normalizeConnectorFamily maps a connector type onto the store whose naming it uses.
func normalizeConnectorFamily(connectorType string) string {
	t := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(connectorType)), "-", "_")
	switch t {
	case "postgresql", "postgres", "pg", "cockroachdb", "cockroach_db", "aurora_postgresql", "alloydb", "neon", "supabase":
		return "postgres"
	case "redshift":
		return "redshift"
	case "mysql", "mariadb", "aurora_mysql":
		return "mysql"
	case "sqlserver", "mssql", "sql_server", "azure_sql":
		return "mssql"
	case "oracle":
		return "oracle"
	case "mongodb", "mongo":
		return "mongodb"
	case "bigquery", "bq", "google_bigquery":
		return "bigquery"
	case "snowflake":
		return "snowflake"
	}
	return ""
}

// openLineagePort reads the port, a number or a string in the settings; 0 when absent.
func openLineagePort(cfg map[string]any) int {
	for _, k := range openLineageConfigKeys.Port {
		switch v := cfg[k].(type) {
		case float64:
			if v > 0 && v < 65536 {
				return int(v)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < 65536 {
				return n
			}
		}
	}
	return 0
}

// openLineageEventRow is one lineage_events row with what the event names besides it.
type openLineageEventRow struct {
	ID          string
	WorkspaceID string
	JobKind     string
	JobID       string
	JobName     string
	RunRef      string
	EventType   string
	StartedAt   *time.Time
	EndedAt     time.Time
	Inputs      []lineageDataset
	Outputs     []lineageDataset
	// ModelConnectionID is a model's connection: the one its column lineage's source
	// tables are on.
	ModelConnectionID string
	// Columns is a model's column lineage for the SQL the run used, nil when not read.
	Columns *savedColumnLineage
}

// The RunEvent, as much of spec 2-0-2 as rsync.ai fills.
type openLineageRunEvent struct {
	EventType string             `json:"eventType"`
	EventTime string             `json:"eventTime"`
	Run       openLineageRun     `json:"run"`
	Job       openLineageJob     `json:"job"`
	Inputs    []openLineageInput `json:"inputs"`
	Outputs   []openLineageInput `json:"outputs"`
	Producer  string             `json:"producer"`
	SchemaURL string             `json:"schemaURL"`
}

type openLineageFacetHead struct {
	Producer  string `json:"_producer"`
	SchemaURL string `json:"_schemaURL"`
}

type openLineageRun struct {
	RunID  string               `json:"runId"`
	Facets openLineageRunFacets `json:"facets"`
}

type openLineageRunFacets struct {
	NominalTime *openLineageNominalTime `json:"nominalTime,omitempty"`
}

type openLineageNominalTime struct {
	openLineageFacetHead
	NominalStartTime string `json:"nominalStartTime"`
	NominalEndTime   string `json:"nominalEndTime"`
}

type openLineageJob struct {
	Namespace string               `json:"namespace"`
	Name      string               `json:"name"`
	Facets    openLineageJobFacets `json:"facets"`
}

type openLineageJobFacets struct {
	JobType openLineageJobType `json:"jobType"`
}

type openLineageJobType struct {
	openLineageFacetHead
	ProcessingType string `json:"processingType"`
	Integration    string `json:"integration"`
	JobType        string `json:"jobType"`
}

// openLineageInput is an input or an output dataset; only an output has facets.
type openLineageInput struct {
	Namespace string                   `json:"namespace"`
	Name      string                   `json:"name"`
	Facets    *openLineageOutputFacets `json:"facets,omitempty"`
}

type openLineageOutputFacets struct {
	Schema        *openLineageSchemaFacet `json:"schema,omitempty"`
	ColumnLineage *openLineageColumnFacet `json:"columnLineage,omitempty"`
}

type openLineageSchemaFacet struct {
	openLineageFacetHead
	Fields []openLineageSchemaField `json:"fields"`
}

// openLineageSchemaField is a column's name. Its type is not known to the lineage
// read, so it is left out, as the facet allows.
type openLineageSchemaField struct {
	Name string `json:"name"`
}

type openLineageColumnFacet struct {
	openLineageFacetHead
	Fields  map[string]openLineageColumnInputs `json:"fields"`
	Dataset []openLineageInputField            `json:"dataset,omitempty"`
}

type openLineageColumnInputs struct {
	InputFields []openLineageInputField `json:"inputFields"`
}

type openLineageInputField struct {
	Namespace       string                       `json:"namespace"`
	Name            string                       `json:"name"`
	Field           string                       `json:"field"`
	Transformations []openLineageTransformations `json:"transformations"`
}

type openLineageTransformations struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

func openLineageHead(url string) openLineageFacetHead {
	return openLineageFacetHead{Producer: openLineageProducer, SchemaURL: url}
}

func openLineageTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// openLineageIsCDCWindow says a pipeline event is a CDC window (cdcTrailingEdgeOccurrence).
func openLineageIsCDCWindow(runRef string) bool { return strings.HasPrefix(runRef, "cdc-trail-") }

// buildOpenLineageEvent builds one RunEvent. stores names each connection's tables; a
// connection it does not have gets the rsync.ai namespace.
func buildOpenLineageEvent(r openLineageEventRow, stores map[string]openLineageStore) openLineageRunEvent {
	store := func(connectionID string) openLineageStore {
		if s, ok := stores[connectionID]; ok {
			return s
		}
		return openLineageConnectionStore(connectionID)
	}
	dataset := func(d lineageDataset) openLineageInput {
		if d.Captured {
			// Its name is the source side's, not where it landed: no store naming.
			return openLineageInput{Namespace: openLineageRsyncNamespace + d.ConnectionID, Name: d.Name}
		}
		s := store(d.ConnectionID)
		return openLineageInput{Namespace: s.Namespace, Name: s.name(d.Name)}
	}

	processing, jobType := "BATCH", "MODEL"
	if r.JobKind == lineageJobPipeline {
		jobType = "PIPELINE"
		if openLineageIsCDCWindow(r.RunRef) {
			processing = "STREAMING"
		}
	}
	ev := openLineageRunEvent{
		EventType: r.EventType,
		EventTime: openLineageTime(r.EndedAt),
		Run:       openLineageRun{RunID: r.ID},
		Job: openLineageJob{
			Namespace: openLineageJobNamespaceHead + r.WorkspaceID,
			Name:      openLineageJobName(r.JobName, r.JobKind, r.JobID),
			Facets: openLineageJobFacets{JobType: openLineageJobType{
				openLineageFacetHead: openLineageHead(openLineageJobTypeURL),
				ProcessingType:       processing, Integration: openLineageIntegration, JobType: jobType,
			}},
		},
		Inputs:    []openLineageInput{},
		Outputs:   []openLineageInput{},
		Producer:  openLineageProducer,
		SchemaURL: openLineageSchemaURL,
	}
	if r.StartedAt != nil {
		ev.Run.Facets.NominalTime = &openLineageNominalTime{
			openLineageFacetHead: openLineageHead(openLineageNominalTimeURL),
			NominalStartTime:     openLineageTime(*r.StartedAt),
			NominalEndTime:       openLineageTime(r.EndedAt),
		}
	}
	for _, d := range r.Inputs {
		ev.Inputs = append(ev.Inputs, dataset(d))
	}
	facets := openLineageModelFacets(r, store)
	for _, d := range r.Outputs {
		out := dataset(d)
		if facets != nil && !d.Captured {
			out.Facets = facets
		}
		ev.Outputs = append(ev.Outputs, out)
	}
	return ev
}

// openLineageJobName is a job's name with its kind and id, so two models of one name
// stay two jobs, and a rename shows as the same id.
func openLineageJobName(name, kind, id string) string {
	if strings.TrimSpace(name) == "" {
		return kind + " " + id
	}
	return name + " (" + kind + " " + id + ")"
}

// openLineageModelFacets builds a model output's schema and column lineage from the
// column lineage read for the SQL the run used; nil when there is none.
func openLineageModelFacets(r openLineageEventRow, store func(string) openLineageStore) *openLineageOutputFacets {
	if r.JobKind != lineageJobModel || r.Columns == nil || len(r.Columns.Columns) == 0 {
		return nil
	}
	f := &openLineageOutputFacets{Schema: &openLineageSchemaFacet{
		openLineageFacetHead: openLineageHead(openLineageSchemaFacetURL),
		Fields:               []openLineageSchemaField{},
	}}
	for _, c := range r.Columns.Columns {
		f.Schema.Fields = append(f.Schema.Fields, openLineageSchemaField{Name: c})
	}
	col := &openLineageColumnFacet{
		openLineageFacetHead: openLineageHead(openLineageColumnFacetURL),
		Fields:               map[string]openLineageColumnInputs{},
	}
	s := store(r.ModelConnectionID)
	for _, e := range r.Columns.Edges {
		if len(e.Table) == 0 || e.Column == "" {
			continue
		}
		in := openLineageInputField{
			Namespace:       s.Namespace,
			Name:            s.name(strings.ToLower(strings.Join(e.Table, "."))),
			Field:           e.Column,
			Transformations: []openLineageTransformations{{Type: e.Type, Subtype: e.Subtype}},
		}
		if e.Type == "DIRECT" && e.ToColumn != "" {
			fc := col.Fields[e.ToColumn]
			fc.InputFields = append(fc.InputFields, in)
			col.Fields[e.ToColumn] = fc
		} else if e.Type != "DIRECT" {
			col.Dataset = append(col.Dataset, in)
		}
	}
	if len(col.Fields) > 0 || len(col.Dataset) > 0 {
		f.ColumnLineage = col
	}
	return f
}
