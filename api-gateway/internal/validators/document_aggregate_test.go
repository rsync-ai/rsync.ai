package validators

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func aggregate(t *testing.T, pipeline string) (*DocumentAggregateSpec, *DocumentFindError) {
	t.Helper()
	return ValidateDocumentAggregate(DocumentAggregateRequest{Collection: "orders", Pipeline: rawJSON(pipeline)})
}

func TestValidateDocumentAggregate_AcceptsAndKeepsKeyOrder(t *testing.T) {
	// $sort's key order is its meaning, and 2^63-1 must not round through float64:
	// the spec forwards the request's own bytes, compacted, never re-encoded.
	in := `[ {"$match": {"n": {"$gt": 9223372036854775807}}},
	         {"$sort": {"z": 1, "a": -1}},
	         {"$lookup": {"from": "users", "localField": "u", "foreignField": "_id", "as": "user",
	                      "pipeline": [{"$project": {"name": 1}}]}},
	         {"$graphLookup": {"from": "users", "startWith": "$m", "connectFromField": "m", "connectToField": "_id", "as": "chain"}},
	         {"$unionWith": "archive"}, {"$unionWith": {"coll": "archive", "pipeline": [{"$limit": 1}]}},
	         {"$facet": {"by_status": [{"$sortByCount": "$status"}], "top": [{"$limit": 1}]}} ]`
	spec, ferr := aggregate(t, in)
	if ferr != nil {
		t.Fatalf("rejected: %+v", ferr)
	}
	got := string(spec.Pipeline)
	if !strings.Contains(got, `{"$sort":{"z":1,"a":-1}}`) || !strings.Contains(got, "9223372036854775807") {
		t.Fatalf("pipeline was re-encoded: %s", got)
	}
	if spec.Limit != DocumentFindDefaultLimit || spec.Collection != "orders" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec, _ := ValidateDocumentAggregate(DocumentAggregateRequest{Collection: "c", Pipeline: rawJSON(`[]`), Limit: 10_000}); spec.Limit != DocumentFindMaxLimit {
		t.Fatalf("limit = %d, want it clamped to %d", spec.Limit, DocumentFindMaxLimit)
	}
	// Every allowlisted stage passes validation (the server checks each one's body).
	for stage := range DocumentAggregateStages {
		switch stage {
		case "$lookup", "$graphLookup", "$unionWith", "$facet":
			continue
		}
		if _, ferr := aggregate(t, fmt.Sprintf(`[{%q: {}}]`, stage)); ferr != nil {
			t.Errorf("%s refused: %+v", stage, ferr)
		}
	}
}

// The connector's test_aggregate_refuses_write_and_code_operators_at_any_depth,
// case for case: the same code and the same path.
func TestValidateDocumentAggregate_RefusesWriteAndCodeOperatorsAtAnyDepth(t *testing.T) {
	const secret = "ssn-123-45-6789"
	cases := []struct{ pipeline, path string }{
		{`[{"$out": "x"}]`, "pipeline[0].$out"},
		{`[{"$merge": {"into": "x"}}]`, "pipeline[0].$merge"},
		{`[{"$match": {"$where": "SECRET"}}]`, "pipeline[0].$match.$where"},
		{`[{"$match": {"$expr": {"$function": {"body": "SECRET", "args": [], "lang": "js"}}}}]`, "pipeline[0].$match.$expr.$function"},
		{`[{"$group": {"_id": null, "x": {"$accumulator": {"init": "SECRET"}}}}]`, "pipeline[0].$group.x.$accumulator"},
		{`[{"$facet": {"a": [{"$match": {}}], "b": [{"$out": "SECRET"}]}}]`, "pipeline[0].$facet.b[0].$out"},
		{`[{"$lookup": {"from": "users", "as": "u", "pipeline": [{"$merge": "SECRET"}]}}]`, "pipeline[0].$lookup.pipeline[0].$merge"},
		{`[{"$unionWith": {"coll": "users", "pipeline": [{"$match": {"$where": "SECRET"}}]}}]`, "pipeline[0].$unionWith.pipeline[0].$match.$where"},
		{`[{"$match": {"a": [{"b": {"$function": "SECRET"}}]}}]`, "pipeline[0].$match.a[0].b.$function"},
	}
	for _, tc := range cases {
		_, ferr := aggregate(t, strings.ReplaceAll(tc.pipeline, "SECRET", secret))
		if ferr == nil || ferr.Code != "operator_not_allowed" || ferr.Path != tc.path {
			t.Errorf("%s: got %+v, want operator_not_allowed at %s", tc.pipeline, ferr, tc.path)
			continue
		}
		if strings.Contains(ferr.Message+ferr.Path, secret) {
			t.Errorf("value echoed: %+v", ferr)
		}
	}
}

func TestValidateDocumentAggregate_StageAllowlistAndShape(t *testing.T) {
	for _, stage := range []string{"$currentOp", "$collStats", "$indexStats", "$listSessions", "$changeStream",
		"$documents", "$planCacheStats", "$listLocalSessions", "$shardedDataDistribution"} {
		_, ferr := aggregate(t, fmt.Sprintf(`[{%q: {}}]`, stage))
		if ferr == nil || ferr.Code != "stage_not_allowed" || ferr.Path != "pipeline[0]."+stage {
			t.Errorf("%s: got %+v, want stage_not_allowed", stage, ferr)
		}
	}
	for _, bad := range []string{`null`, `{"$match": {}}`, `["$match"]`, `[{"$match": {}, "$limit": 1}]`, `[{}]`,
		`[{"$facet": {}}]`, `[{"$facet": {"$x": []}}]`, `[{"$facet": {"a": {"$match": {}}}}]`, `[{"$lookup": "users"}]`, `[`} {
		_, ferr := aggregate(t, bad)
		if ferr == nil || ferr.Code != "invalid_pipeline" {
			t.Errorf("%s: got %+v, want invalid_pipeline", bad, ferr)
		}
	}
}

func TestValidateDocumentAggregate_ForeignCollectionsFollowFindRules(t *testing.T) {
	cases := []struct{ stage, path string }{
		{`{"$lookup": {"from": "system.users", "as": "u"}}`, "pipeline[0].$lookup.from"},
		{`{"$lookup": {"from": {"db": "admin", "coll": "x"}, "as": "u"}}`, "pipeline[0].$lookup.from"},
		{`{"$lookup": {"as": "u", "pipeline": []}}`, "pipeline[0].$lookup.from"},
		{`{"$graphLookup": {"from": "system.profile", "as": "g"}}`, "pipeline[0].$graphLookup.from"},
		{`{"$unionWith": "system.js"}`, "pipeline[0].$unionWith"},
		{`{"$unionWith": {"coll": "a$b"}}`, "pipeline[0].$unionWith.coll"},
	}
	for _, tc := range cases {
		_, ferr := aggregate(t, "["+tc.stage+"]")
		if ferr == nil || ferr.Code != "invalid_collection" || ferr.Path != tc.path {
			t.Errorf("%s: got %+v, want invalid_collection at %s", tc.stage, ferr, tc.path)
		}
	}
}

func TestValidateDocumentAggregate_Limits(t *testing.T) {
	stages := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"$skip": 0},`, n), ",") + "]"
	}
	if _, ferr := aggregate(t, stages(DocumentAggregateMaxStages)); ferr != nil {
		t.Fatalf("%d stages refused: %+v", DocumentAggregateMaxStages, ferr)
	}
	if _, ferr := aggregate(t, stages(DocumentAggregateMaxStages+1)); ferr == nil || ferr.Code != "pipeline_too_long" {
		t.Fatalf("got %+v, want pipeline_too_long", ferr)
	}
	deep := "0"
	for i := 0; i < DocumentAggregateMaxDepth+5; i++ {
		deep = `{"a": ` + deep + `}`
	}
	if _, ferr := aggregate(t, `[{"$match": `+deep+`}]`); ferr == nil || ferr.Code != "pipeline_too_deep" {
		t.Fatalf("got %+v, want pipeline_too_deep", ferr)
	}
	facets := `[{"$limit": 1}]`
	for i := 0; i < DocumentAggregateMaxDepth; i++ {
		facets = `[{"$facet": {"f": ` + facets + `}}]`
	}
	if _, ferr := aggregate(t, facets); ferr == nil || ferr.Code != "pipeline_too_deep" {
		t.Fatalf("got %+v, want pipeline_too_deep for nested $facet", ferr)
	}
	big, _ := json.Marshal([]interface{}{map[string]interface{}{"$match": map[string]interface{}{
		"status": map[string]interface{}{"$in": repeatString(strings.Repeat("x", 1000), 140)}}}})
	if _, ferr := aggregate(t, string(big)); ferr == nil || ferr.Code != "pipeline_too_large" {
		t.Fatalf("got %+v, want pipeline_too_large", ferr)
	}
	if _, ferr := aggregate(t, `[{"$match": {"_id": {"$oid": "0123456789abcdef01234567", "x": 1}}}]`); ferr == nil ||
		ferr.Code != "invalid_pipeline" || ferr.Path != "pipeline[0].$match._id.$oid" {
		t.Fatalf("got %+v, want invalid_pipeline at the $oid wrapper", ferr)
	}
}

func repeatString(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// The lockstep with connector.py: the stage allowlist and the denied operators are
// read out of the connector the running image is built from (versions/<current>/), so
// a stage allowed on one side and not the other fails here, not in production.
func TestDocumentAggregateListsMatchConnector(t *testing.T) {
	root := filepath.Join("..", "..", "..", "shared", "mcp-connectors", "public", "database", "mongodb")
	b, err := os.ReadFile(filepath.Join(root, "latest.json"))
	if err != nil {
		t.Fatalf("mongodb latest.json not found (err=%v)", err) // Fatal, not Skip: a skipped guard reads as a pass
	}
	var manifest struct {
		CurrentVersion string `json:"current_version"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil || manifest.CurrentVersion == "" {
		t.Fatalf("mongodb latest.json has no current_version (err=%v)", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "versions", manifest.CurrentVersion, "connector.py"))
	if err != nil {
		t.Fatalf("connector.py: %v", err)
	}
	pySet := func(name string) []string {
		m := regexp.MustCompile(`(?s)\n` + name + ` = frozenset\(\{(.*?)\}\)`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in connector.py", name)
		}
		var out []string
		for _, q := range regexp.MustCompile(`"(\$\w+)"`).FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
		sort.Strings(out)
		return out
	}
	goSet := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		name string
		goes map[string]bool
	}{{"AGGREGATE_STAGES", DocumentAggregateStages}, {"AGGREGATE_DENIED_OPERATORS", DocumentAggregateDeniedOperators}} {
		py, gs := pySet(c.name), goSet(c.goes)
		if len(py) == 0 || strings.Join(py, ",") != strings.Join(gs, ",") {
			t.Errorf("%s drifted:\n connector.py: %v\n gateway:      %v", c.name, py, gs)
		}
	}
	for _, c := range []struct {
		name string
		want int
	}{{"AGGREGATE_MAX_STAGES", DocumentAggregateMaxStages}, {"AGGREGATE_MAX_DEPTH", DocumentAggregateMaxDepth}} {
		if !regexp.MustCompile(`\n` + c.name + fmt.Sprintf(` = %d\n`, c.want)).Match(src) {
			t.Errorf("%s in connector.py is not %d", c.name, c.want)
		}
	}
	if !regexp.MustCompile(fmt.Sprintf(`\nAGGREGATE_MAX_PIPELINE_BYTES = %d \* 1024\n`, DocumentAggregateMaxPipelineBytes/1024)).Match(src) {
		t.Errorf("AGGREGATE_MAX_PIPELINE_BYTES in connector.py is not %d KB", DocumentAggregateMaxPipelineBytes/1024)
	}
}

// DocumentAggregateSources is what lineage reads a MongoDB model's inputs from. Every
// stage that names a collection counts, at any depth — inside $lookup.pipeline,
// $unionWith.pipeline and $facet — and nothing else does: a $match on a field called
// "from", or a $lookup's "as", is not an input.
func TestDocumentAggregateSources_EveryCollectionAPipelineReads(t *testing.T) {
	spec, ferr := ValidateDocumentAggregate(DocumentAggregateRequest{
		Database: "shop", Collection: "orders",
		Pipeline: rawJSON(`[
			{"$match": {"from": "not_a_source", "coll": "nor_this"}},
			{"$lookup": {"from": "customers", "localField": "c", "foreignField": "_id", "as": "buyer"}},
			{"$lookup": {"from": "items", "let": {"o": "$_id"}, "as": "i", "pipeline": [
				{"$lookup": {"from": "products", "localField": "p", "foreignField": "_id", "as": "p"}}
			]}},
			{"$graphLookup": {"from": "categories", "startWith": "$cat", "connectFromField": "parent",
				"connectToField": "_id", "as": "tree"}},
			{"$unionWith": "returns"},
			{"$unionWith": {"coll": "archive.orders_2025", "pipeline": [{"$unionWith": "refunds"}]}},
			{"$facet": {
				"z": [{"$lookup": {"from": "zones", "localField": "z", "foreignField": "_id", "as": "z"}}],
				"a": [{"$unionWith": "adjustments"},
				      {"$lookup": {"from": "customers", "localField": "c", "foreignField": "_id", "as": "again"}}]
			}}
		]`),
	})
	if ferr != nil {
		t.Fatalf("fixture pipeline must validate: %+v", ferr)
	}
	// First appearance, each once; $facet branches in name order so the answer is stable.
	want := []string{"orders", "customers", "items", "products", "categories", "returns",
		"archive.orders_2025", "refunds", "adjustments", "zones"}
	if got := DocumentAggregateSources(spec); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources:\n got %v\nwant %v", got, want)
	}
	if got := DocumentAggregateSources(nil); got != nil {
		t.Fatalf("no spec reads nothing, got %v", got)
	}
}
