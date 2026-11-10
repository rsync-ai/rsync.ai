package transforms

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func maskStep(cfg map[string]any) CanonicalTransform {
	return CanonicalTransform{Type: "mask_pii", Enabled: true, Config: cfg}
}

func runMaskStep(t *testing.T, in []Row, step CanonicalTransform) []Row {
	t.Helper()
	out, err := NewSimpleTransformEngine().Apply(context.Background(), in, step.EngineTransform())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The class: a mask that cannot reach the place a value sits leaves it in
// plaintext and reports success. A top-level column mask on a row with the same
// field nested is that class on a MongoDB batch row.
func TestVerifyMaskStep_NestedLeftoverUnderTopLevelMask(t *testing.T) {
	step := maskStep(map[string]any{"column": "email"})
	in := []Row{{
		"email":   "alice@example.com",
		"profile": map[string]interface{}{"Email": "alice.home@example.com"},
	}}
	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := VerifyMaskStep(snap, runMaskStep(t, in, step))
	if !errors.Is(err, ErrResidualPlaintext) {
		t.Fatalf("err = %v, want ErrResidualPlaintext", err)
	}
	if !reflect.DeepEqual(targets, []string{"email"}) {
		t.Fatalf("targets = %v, want [email]", targets)
	}
	if !strings.Contains(err.Error(), "1 of 1 row(s)") || !strings.Contains(err.Error(), `"deep": true`) {
		t.Fatalf("error should count rows and say how to fix it: %v", err)
	}
	if strings.Contains(err.Error(), "alice") {
		t.Fatalf("error carries a row value: %v", err)
	}
}

func TestVerifyMaskStep_EveryMaskTypePassesWhenItReaches(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"column": "email", "deep": true},
		{"column": "email", "deep": true, "mask_type": "redact"},
		{"column": "email", "deep": true, "mask_type": "partial"},
		{"column": "email", "deep": true, "mask_type": "remove"},
		{"column": "email", "deep": true, "mask_type": "hash", "hash_function": "md5"},
		{"column": "contact", "deep": true},
		{"path": "profile.email"},
		{"path": "cards.email"},
	} {
		step := maskStep(cfg)
		in := []Row{{
			"email":   "alice@example.com",
			"contact": map[string]interface{}{"phone": "+44 20 7946 0000"},
			"profile": map[string]interface{}{"email": "alice.home@example.com"},
			"cards":   []interface{}{map[string]interface{}{"email": "card@example.com"}},
		}}
		snap, err := SnapshotMaskStep(in, step)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Values() == 0 {
			t.Fatalf("control: %v collected nothing, the pass below would be vacuous", cfg)
		}
		if targets, err := VerifyMaskStep(snap, runMaskStep(t, in, step)); err != nil || len(targets) != 0 {
			t.Fatalf("%v: masked output flagged: %v %v", cfg, targets, err)
		}
	}
}

// Two rows sharing a value, and a column that merely equals the masked one, are
// not leaks of the mask: the step guard compares a row with its own input and
// only where a target reaches. The whole-batch guard flags both, which is what
// makes it the wrong check to wrap around one step.
func TestVerifyMaskStep_OtherRowsAndUntargetedColumnsAreNotLeaks(t *testing.T) {
	step := maskStep(map[string]any{"column": "email"})
	in := []Row{
		{"email": "alice@example.com", "referrer": "bob@example.com"},
		{"email": "bob@example.com", "username": "bob@example.com"},
	}
	out := runMaskStep(t, in, step)

	batch, err := SnapshotPlaintext(in, []CanonicalTransform{step})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := VerifyNoResidualPlaintext(batch, out); err == nil || len(v) != 2 {
		t.Fatalf("control: the whole-batch guard should flag referrer and username, got %v %v", v, err)
	}

	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		t.Fatal(err)
	}
	if targets, err := VerifyMaskStep(snap, out); err != nil || len(targets) != 0 {
		t.Fatalf("step guard flagged a value the mask was not asked to reach: %v %v", targets, err)
	}
}

func TestVerifyMaskStep_PathTargetLeftover(t *testing.T) {
	step := maskStep(map[string]any{"paths": []interface{}{"document.contact.email", "document.ssn"}})
	in := []Row{
		{"_id": "a1", "document": map[string]interface{}{"contact": map[string]interface{}{"email": "alice@example.com"}}},
		{"_id": "a2", "document": map[string]interface{}{"ssn": "123-45-6789"}},
	}
	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		t.Fatal(err)
	}
	// The input itself stands in for a mask that did nothing.
	targets, err := VerifyMaskStep(snap, in)
	if !errors.Is(err, ErrResidualPlaintext) {
		t.Fatalf("err = %v, want ErrResidualPlaintext", err)
	}
	if !reflect.DeepEqual(targets, []string{"document.contact.email", "document.ssn"}) {
		t.Fatalf("targets = %v", targets)
	}
	if !strings.Contains(err.Error(), "2 of 2 row(s)") {
		t.Fatalf("row count missing: %v", err)
	}
}

// A document that uses data as keys would put that data in an output path.
// The error names targets only, so the key never reaches run history, the DLQ
// or the AI diagnosis.
func TestVerifyMaskStep_ErrorNeverCarriesDataUsedAsKeys(t *testing.T) {
	step := maskStep(map[string]any{"column": "email"})
	in := []Row{{"contacts": map[string]interface{}{
		"Jane Doe": map[string]interface{}{"email": "jane@example.com"},
	}}}
	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMaskStep(snap, runMaskStep(t, in, step))
	if !errors.Is(err, ErrResidualPlaintext) {
		t.Fatalf("err = %v, want ErrResidualPlaintext", err)
	}
	for _, leak := range []string{"Jane", "jane", "contacts"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error carries %q: %v", leak, err)
		}
	}
}

func TestVerifyMaskStep_FailsClosed(t *testing.T) {
	if _, err := VerifyMaskStep(nil, nil); err == nil {
		t.Fatal("nil snapshot must be an error")
	}
	step := maskStep(map[string]any{"column": "email"})
	in := []Row{{"email": "alice@example.com"}, {"email": "bob@example.com"}}
	snap, err := SnapshotMaskStep(in, step)
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMaskStep(snap, in[:1])
	if err == nil || errors.Is(err, ErrResidualPlaintext) {
		t.Fatalf("a changed row count must fail as its own error, got %v", err)
	}
}

func TestSnapshotMaskStep_IgnoresOtherSteps(t *testing.T) {
	snap, err := SnapshotMaskStep([]Row{{"email": "alice@example.com"}},
		CanonicalTransform{Type: "filter", Enabled: true, Config: map[string]any{"condition": "id > 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Values() != 0 {
		t.Fatalf("a filter step has no mask targets, collected %d", snap.Values())
	}
}
