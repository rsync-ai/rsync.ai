package transforms

import (
	"reflect"
	"sort"
	"testing"
)

func TestRulesUnsupportedOnPackedDocument(t *testing.T) {
	chain := []CanonicalTransform{
		{Type: "mask_pii", Enabled: true},
		{Type: "type_convert", Enabled: true},
		{Type: "filter", Enabled: true},
		{Type: "select_columns", Enabled: true},
		{Type: "filter", Enabled: true},
		{Type: "exclude_columns", Enabled: false},
	}
	if got := RulesUnsupportedOnPackedDocument(chain); !reflect.DeepEqual(got, []string{"filter", "select_columns"}) {
		t.Fatalf("got %v, want [filter select_columns]", got)
	}
	if got := RulesUnsupportedOnPackedDocument(chain[:2]); len(got) != 0 {
		t.Fatalf("mask_pii + type_convert both run on a packed row, got %v", got)
	}
}

// Every type the engine runs is classified, so a new type cannot reach a packed
// MongoDB row without someone deciding whether it can run there.
func TestRunsOnPackedDocument_ClassifiesEverySupportedType(t *testing.T) {
	want := map[string]bool{"mask_pii": true, "type_convert": true}
	for _, tt := range SupportedTransformTypes() {
		if RunsOnPackedDocument(tt) != want[tt] {
			t.Errorf("%s: RunsOnPackedDocument = %v, want %v", tt, !want[tt], want[tt])
		}
	}
	for _, alias := range []string{"mask", "hash", "cast", "type_cast"} {
		if !RunsOnPackedDocument(alias) {
			t.Errorf("alias %s should resolve to a type that runs on a packed row", alias)
		}
	}
}

// The list a UI offers is exactly the types that run: nothing it offers is then
// refused on save, and nothing that runs is left out.
func TestPackedDocumentRuleTypes_AreExactlyTheTypesThatRun(t *testing.T) {
	got := PackedDocumentRuleTypes()
	for _, tt := range got {
		if !RunsOnPackedDocument(tt) {
			t.Errorf("%s is offered but does not run on a packed row", tt)
		}
	}
	var want []string
	for _, tt := range SupportedTransformTypes() {
		if RunsOnPackedDocument(tt) {
			want = append(want, tt)
		}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PackedDocumentRuleTypes() = %v, want %v", got, want)
	}
}

func TestIsMongoSourceType(t *testing.T) {
	for _, s := range []string{"mongodb", "MongoDB", " mongo ", "mongodb-atlas", "mongodb_atlas", "mongodbatlas", "atlas"} {
		if !IsMongoSourceType(s) {
			t.Errorf("%q should be MongoDB-family", s)
		}
	}
	for _, s := range []string{"", "postgresql", "documentdb", "mongodb-cdc"} {
		if IsMongoSourceType(s) {
			t.Errorf("%q should not be MongoDB-family", s)
		}
	}
}
