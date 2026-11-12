package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rsync-ai/shared/transforms"
)

// The bug class these tests cover: a consumer rule written for a flat row meets
// a packed MongoDB CDC row, {_id, document}, and does nothing while the event
// reports success. mask_pii was fixed in #1462; type_convert now runs inside
// the document too; every other rule is refused; and every mask runs under the
// residual-plaintext guard.

func TestMongoCDCRules_TypeConvertRunsInsideTheDocument(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	packedDocument(t, sm.Data[0])["age"] = "42"
	chain := mongoMaskChain(t,
		map[string]any{"operation": "type_convert", "column": "age", "to": "integer"},
		emailMask(),
	)
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	row := sm.Data[0]
	doc := packedDocument(t, row)
	if age, ok := doc["age"].(int); !ok || age != 42 {
		t.Fatalf("document.age = %#v (%T), want int 42", doc["age"], doc["age"])
	}
	if len(row) != 2 {
		t.Fatalf("packed row grew columns of its own: %v", row)
	}
	if out := mongoMaskSerialized(t, sm.Data); strings.Contains(out, mongoMaskTopEmail) {
		t.Fatalf("the mask after the type_convert did not run: %s", out)
	}
}

// A rule that names a top-level column cannot reach a document field, and on
// a packed row it used to pass every row (filter) or empty each one (select)
// without a word. It is refused, for every operation, so the event goes to the
// DLQ and the operator sees why.
func TestMongoCDCRules_RefusesRulesThatCannotReachTheDocument(t *testing.T) {
	// Configs are built per run: normalizing a rule edits its config map.
	for _, tc := range []struct {
		name string
		cfg  func() map[string]any
	}{
		{"filter", func() map[string]any { return map[string]any{"operation": "filter", "condition": "status = 'active'"} }},
		{"select_columns", func() map[string]any { return map[string]any{"operation": "select_columns", "columns": []any{"email"}} }},
		{"exclude_columns", func() map[string]any {
			return map[string]any{"operation": "exclude_columns", "columns": []any{"email"}}
		}},
		{"rename_columns", func() map[string]any {
			return map[string]any{"operation": "rename_columns", "mappings": map[string]any{"email": "contact_email"}}
		}},
	} {
		for _, op := range []string{"c", "d"} {
			t.Run(tc.name+"/op="+op, func(t *testing.T) {
				sm := mongoMaskEvent(t, op)
				err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, emailMask(), tc.cfg()))
				if !errors.Is(err, errPackedMongoRuleUnsupported) {
					t.Fatalf("err = %v, want errPackedMongoRuleUnsupported", err)
				}
				if !strings.Contains(err.Error(), tc.name) {
					t.Fatalf("the error should name the rule type: %v", err)
				}
				if sm.consumerTransformsApplied {
					t.Fatal("a refused event must not be marked as transformed")
				}
			})
		}
	}
}

// A mask whose deep matching was switched off cannot reach the nested copies;
// the guard stops the event instead of shipping them.
func TestMongoCDCMask_GuardStopsAMaskThatCannotReach(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	chain := mongoMaskChain(t, map[string]any{"operation": "mask_pii", "column": "email", "deep": false})
	err := applyCDCTransformChain(context.Background(), nil, sm, chain)
	if !errors.Is(err, transforms.ErrResidualPlaintext) {
		t.Fatalf("err = %v, want ErrResidualPlaintext", err)
	}
	if strings.Contains(err.Error(), "example.com") {
		t.Fatalf("the error carries a row value: %v", err)
	}

	// A path that reaches the nested copy passes.
	ok := mongoMaskEvent(t, "c")
	pathChain := mongoMaskChain(t, map[string]any{"operation": "mask_pii", "paths": []any{"document.email", "document.contact.Email", "document.cards.email"}})
	if err := applyCDCTransformChain(context.Background(), nil, ok, pathChain); err != nil {
		t.Fatalf("paths that reach every copy: %v", err)
	}
}

// The packed row's _id is the sink's key, as #1462 decided: a mask on "_id"
// runs inside the document and the guard checks the documents, so the key
// column is not a leak that stops the stream.
func TestMongoCDCMask_AMaskOnIDLeavesTheSinkKey(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	key := sm.Data[0]["_id"]
	if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t,
		map[string]any{"operation": "mask_pii", "column": "_id", "mask_type": "hash"})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sm.Data[0]["_id"] != key {
		t.Fatalf("_id = %v, want the sink key %v unchanged", sm.Data[0]["_id"], key)
	}
}

// A CDC stream has no end of run, so a target that has matched nothing is
// logged once it has seen maskUnmatchedWarnRows rows, and only once.
func TestNoteMaskMatches_WarnsOncePerTargetAfterEnoughRows(t *testing.T) {
	half := maskUnmatchedWarnRows/2 + 1
	stats := transforms.MaskStats{Rows: half, Targets: []transforms.MaskTargetStats{
		{Target: "ssn", Kind: "deep"}, {Target: "email", Kind: "deep", Matched: half},
	}}
	pid := "pipeline-note-mask-matches"
	if due := noteMaskMatches(pid, "customers", 1, stats); len(due) != 0 {
		t.Fatalf("warned after %d rows: %v", half, due)
	}
	due := noteMaskMatches(pid, "customers", 1, stats)
	want := []maskMatchKey{{pipeline: pid, table: "customers", order: 1, target: "ssn"}}
	if !reflect.DeepEqual(due, want) {
		t.Fatalf("due = %+v, want %+v", due, want)
	}
	if due := noteMaskMatches(pid, "customers", 1, stats); len(due) != 0 {
		t.Fatalf("warned twice: %v", due)
	}
}

// The chain reports the unmatched target through the structured log: names and
// counts, never a value.
func TestMongoCDCMask_LogsAMaskThatMatchesNothing(t *testing.T) {
	cfg := func() map[string]any {
		return map[string]any{"operation": "mask_pii", "columns": []any{"email", "national_id"}}
	}
	var lines []map[string]any
	lines = captureLogLines(t, func() {
		for i := 0; i < maskUnmatchedWarnRows; i++ {
			sm := mongoMaskEvent(t, "c")
			sm.PipelineID = "pipeline-logs-unmatched-mask"
			if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, cfg())); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
	})
	var hits []map[string]any
	for _, l := range lines {
		if l["message"] == "mask matched no values; the rule masks nothing" {
			hits = append(hits, l)
		}
	}
	if len(hits) != 1 || hits[0]["target"] != "national_id" || hits[0]["level"] != "warning" {
		t.Fatalf("want one warning for national_id, got %v", hits)
	}
	if strings.Contains(mongoMaskSerialized(t, hits), "example.com") {
		t.Fatalf("the warning carries a row value: %v", hits)
	}
}
