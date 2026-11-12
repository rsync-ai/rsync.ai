package charts

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateChartTypes = flag.Bool("update-chart-types", false, "rewrite shared/chart_types_golden.json from the registry")

const chartTypesGolden = "../../../shared/chart_types_golden.json"

const chartTypesComment = "The chart type registry (dashboards-design.md § 14.2), shared by api-gateway's internal/charts/registry.go and the frontend's components/charts/chart-registry.ts: each type's roles (fewest and most fields, column kinds, field attributes), query path, caps, whether the server draws it for an email or a PDF, and the sorts it takes. registry_test.go and chart-registry.test.ts each compare their registry to this file, so a type, role or cap added on one side alone fails a test. Rewrite from Go: go test ./internal/charts -run TestRegistryMatchesTheSharedGolden -update-chart-types."

// The registry and the frontend's mirror are pinned to one file: a type or a cap added
// on one side alone fails here or in chart-registry.test.ts.
func TestRegistryMatchesTheSharedGolden(t *testing.T) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Comment string     `json:"_comment"`
		Types   []TypeSpec `json:"types"`
	}{chartTypesComment, Registry}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Clean(chartTypesGolden)
	if *updateChartTypes {
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("%s differs from the registry; the registry is:\n%s", path, b.String())
	}
}

// Each type's roles are ones validateShape can name, each attribute one a field has,
// and its caps are set: an entry the checks cannot read would pass settings unchecked.
func TestRegistryEntriesAreComplete(t *testing.T) {
	full := Field{Column: "c", Aggregate: "a", Grain: "g", Title: "t", Format: "f", Color: "c", Colors: []SeriesColor{{}},
		Mark: "m", Axis: "x", Bins: 1, Width: 1}
	attrs := fieldAttrs(full)
	// Each attribute is the JSON key a field saves it under: KeepFrom copies by key.
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(attrs) {
		t.Errorf("a field saves %d keys and names %d attributes", len(keys), len(attrs))
	}
	for _, a := range attrs {
		if _, ok := keys[a]; !ok {
			t.Errorf("attribute %q is not a key a field saves", a)
		}
	}
	seen := map[string]bool{}
	for _, spec := range Registry {
		if seen[spec.Type] {
			t.Errorf("%s is in the registry twice", spec.Type)
		}
		seen[spec.Type] = true
		if spec.Label == "" || spec.Version != Version || spec.Cap.Rows < 1 || spec.Cap.Rows > MaxRows || spec.Sorts == nil {
			t.Errorf("%s: label, version, row cap or sorts not set: %+v", spec.Type, spec)
		}
		if spec.Path != PathAggregate && spec.Path != PathRows && spec.Path != PathStatistics {
			t.Errorf("%s: path %q", spec.Type, spec.Path)
		}
		// A type with a measure names the aggregates it takes, each one rsync.ai offers.
		if spec.Aggregates == nil || (spec.Role(RoleMeasure) != nil) != (len(spec.Aggregates) > 0) {
			t.Errorf("%s: a measure role and its aggregates go together: %v", spec.Type, spec.Aggregates)
		}
		for _, a := range spec.Aggregates {
			if !contains(Aggregates, a) {
				t.Errorf("%s: aggregate %q is not one rsync.ai offers", spec.Type, a)
			}
		}
		for _, r := range spec.Roles {
			if roleNeed[r.Name] == "" || roleLack[r.Name] == "" || !contains(roleOrder, r.Name) {
				t.Errorf("%s: role %q has no words for its refusals", spec.Type, r.Name)
			}
			if r.Min < 0 || r.Max < 1 || r.Min > r.Max || len(r.Kinds) == 0 || len(r.Attrs) == 0 {
				t.Errorf("%s: role %s counts, kinds or attributes: %+v", spec.Type, r.Name, r)
			}
			for _, a := range r.Attrs {
				if !contains(attrs, a) {
					t.Errorf("%s: role %s attribute %q is not a field's", spec.Type, r.Name, a)
				}
			}
		}
		if (spec.Role(RoleSeries) != nil) != (spec.Cap.Series > 0) {
			t.Errorf("%s: a series role and a series cap go together", spec.Type)
		}
	}
	if Spec("gauge") != nil || Spec(TypeBar) == nil {
		t.Fatal("Spec lookup")
	}
}
