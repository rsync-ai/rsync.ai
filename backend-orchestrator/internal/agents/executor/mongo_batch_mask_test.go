package executor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rsync-ai/shared/transforms"
)

// maskTask is a batch task carrying one mask rule in the stored
// transform_definitions shape, from the given source type.
func maskTask(sourceType string, cfg map[string]interface{}) ExecutorTask {
	return ExecutorTask{
		PipelineID: "adhoc-mask-test",
		Source:     &ConnectorConfig{Type: sourceType},
		Params: map[string]interface{}{"metadata": map[string]interface{}{"transforms": []interface{}{
			map[string]interface{}{"id": "r1", "transform_order": 1, "enabled": true, "transform_config": cfg},
		}}},
	}
}

func customerDoc() map[string]interface{} {
	return map[string]interface{}{
		"_id":     "65f0aa01",
		"email":   "alice@example.com",
		"profile": map[string]interface{}{"email": "alice.home@example.com", "tier": "gold"},
		"cards":   []interface{}{map[string]interface{}{"email": "card@example.com"}},
	}
}

// The bug class: a mask rule is the user saying "this field must not leave in
// plaintext", and on a MongoDB batch row a top-level mask left every nested
// copy of the field in plaintext while the run reported success.
func TestApplyTransformsToData_MongoSourceMasksNestedFields(t *testing.T) {
	for _, src := range []string{"mongodb", "mongodb-atlas"} {
		task := maskTask(src, map[string]interface{}{"operation": "mask_pii", "column": "email", "mask_type": "hash"})
		out, err := applyTransformsToData(context.Background(), nil, "exec-1", []map[string]interface{}{customerDoc()}, task, "customers", nil)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		b, _ := json.Marshal(out)
		for _, plain := range []string{"alice@example.com", "alice.home@example.com", "card@example.com"} {
			if strings.Contains(string(b), plain) {
				t.Fatalf("%s: %q left in plaintext: %s", src, plain, b)
			}
		}
		if out[0]["profile"].(map[string]interface{})["tier"] != "gold" {
			t.Fatalf("%s: an unmasked nested field changed: %s", src, b)
		}
	}
}

// A relational source keeps the documented top-level, exact-case behavior.
func TestApplyTransformsToData_RelationalMaskStaysTopLevel(t *testing.T) {
	task := maskTask("postgresql", map[string]interface{}{"operation": "mask_pii", "column": "email", "mask_type": "redact"})
	in := []map[string]interface{}{{"id": 7, "email": "alice@example.com", "Email_Verified": true, "name": "Alice"}}
	out, err := applyTransformsToData(context.Background(), nil, "exec-1", in, task, "public.users", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"id": 7, "email": "***", "Email_Verified": true, "name": "Alice"}
	if !reflect.DeepEqual(out[0], want) {
		t.Fatalf("got %v, want %v", out[0], want)
	}
}

// When a mask cannot reach a field it names — a top-level rule on a nested
// value, from any source, or deep explicitly switched off on MongoDB — the
// step fails instead of shipping the value. The error names the target, never
// the value.
func TestApplyTransformsToData_MaskThatCannotReachFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		src string
		cfg map[string]interface{}
	}{
		{"postgresql", map[string]interface{}{"operation": "mask_pii", "column": "email"}},
		{"mongodb", map[string]interface{}{"operation": "mask_pii", "column": "email", "deep": false}},
	} {
		_, err := applyTransformsToData(context.Background(), nil, "exec-1", []map[string]interface{}{customerDoc()}, maskTask(tc.src, tc.cfg), "customers", nil)
		if !errors.Is(err, transforms.ErrResidualPlaintext) {
			t.Fatalf("%s %v: err = %v, want ErrResidualPlaintext", tc.src, tc.cfg, err)
		}
		if strings.Contains(err.Error(), "example.com") {
			t.Fatalf("error carries a row value: %v", err)
		}
	}
}

// A mask whose target matched nothing in every row masked nothing. The tally
// adds the steps up across tables so the run can say so once.
func TestApplyTransformsToData_TalliesUnmatchedMaskTargets(t *testing.T) {
	tally := newMaskMatchTally()
	for _, table := range []string{"public.users", "public.staff"} {
		// A task per table, as the run loads the rules from transform_definitions
		// for each table: normalizing a rule edits its config map in place.
		task := maskTask("postgresql", map[string]interface{}{"operation": "mask_pii", "columns": []interface{}{"email", "ssn"}})
		in := []map[string]interface{}{{"email": "alice@example.com"}, {"email": "bob@example.com"}}
		if _, err := applyTransformsToData(context.Background(), nil, "exec-1", in, task, table, tally); err != nil {
			t.Fatal(err)
		}
	}
	got := tally.unmatched()
	want := []maskUnmatched{{Order: 1, Target: "ssn", Rows: 4, Tables: []string{"public.staff", "public.users"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatched = %+v, want %+v", got, want)
	}
	var nilTally *maskMatchTally
	nilTally.add(1, "t", transforms.MaskStats{Rows: 1})
	if nilTally.unmatched() != nil {
		t.Fatal("a nil tally must be a no-op")
	}
}

func TestBuildExecutorMaskUnmatchedEvent(t *testing.T) {
	evt := buildExecutorMaskUnmatchedEvent("p1", "e1", "tr1", maskUnmatched{Order: 2, Target: "ssn", Rows: 40, Tables: []string{"public.users"}})
	if evt["severity"] != "warning" || evt["summary"] != "Mask matched no values" || evt["table_name"] != "public.users" {
		t.Fatalf("event = %v", evt)
	}
	msg, _ := evt["message"].(string)
	if !strings.Contains(msg, `"ssn"`) || !strings.Contains(msg, "40 row(s)") {
		t.Fatalf("message = %q", msg)
	}
	if _, ok := evt["progress"]; ok {
		t.Fatal("a warning row carries no progress percent")
	}
	multi := buildExecutorMaskUnmatchedEvent("p1", "e1", "tr1", maskUnmatched{Order: 2, Target: "ssn", Rows: 40, Tables: []string{"a", "b"}})
	if _, ok := multi["table_name"]; ok {
		t.Fatal("a rule that ran on several tables names none of them as the event's table")
	}
}
