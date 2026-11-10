package transforms

import (
	"context"
	"testing"
)

// KI-MASK-REMOVE-KEEPS-THE-COLUMN: the Mask rule offers "Remove", and the engine
// had no remove case, so the default branch wrote "***" and the column stayed.
// Remove has to drop the key at every place a mask can reach: top level, any
// depth (deep) and an explicit path.

func applyMaskRule(t *testing.T, cfg map[string]interface{}, in []Row) ([]Row, MaskStats) {
	t.Helper()
	out, stats, err := NewSimpleTransformEngine().ApplyMaskWithStats(context.Background(), in, cfg)
	if err != nil {
		t.Fatalf("mask %v: %v", cfg, err)
	}
	return out, stats
}

func TestMaskRemove_TopLevelDropsTheColumn(t *testing.T) {
	for _, mt := range []string{"remove", "Remove", " REMOVE "} {
		in := []Row{
			{"id": 1, "country": "IN", "name": "a"},
			{"id": 2, "country": nil, "name": "b"},
			{"id": 3, "name": "c"},
		}
		out, stats := applyMaskRule(t, map[string]interface{}{"column": "country", "mask_type": mt}, in)

		for i, row := range out {
			if v, present := row["country"]; present {
				t.Errorf("mask_type %q row %d still has country=%v", mt, i, v)
			}
			if row["name"] == nil || row["id"] == nil {
				t.Errorf("mask_type %q row %d lost an untouched column: %v", mt, i, row)
			}
		}
		// A null value is still a column the destination would create.
		if got := stats.Targets[0].Matched; got != 2 {
			t.Errorf("mask_type %q matched %d, want 2 (rows that had the key)", mt, got)
		}
		if _, present := in[0]["country"]; !present {
			t.Fatalf("the caller's input row was mutated")
		}
	}
}

func TestMaskRemove_DeepDropsTheNestedKey(t *testing.T) {
	doc := map[string]interface{}{"email": "packed@example.com", "plan": "pro"}
	in := []Row{
		{"_id": "x1", "document": doc},
		{"_id": "x2", "tags": []interface{}{map[string]interface{}{"Email": "arr@example.com", "k": 1}}},
	}
	out, stats := applyMaskRule(t, map[string]interface{}{"column": "email", "deep": true, "mask_type": "remove"}, in)

	gotDoc := out[0]["document"].(map[string]interface{})
	if v, present := gotDoc["email"]; present {
		t.Errorf("deep remove kept document.email=%v", v)
	}
	if gotDoc["plan"] != "pro" {
		t.Errorf("deep remove lost a sibling key: %v", gotDoc)
	}
	el := out[1]["tags"].([]interface{})[0].(map[string]interface{})
	if v, present := el["Email"]; present {
		t.Errorf("deep remove kept tags[0].Email=%v", v)
	}
	if stats.Targets[0].Matched != 2 {
		t.Errorf("matched %d, want 2", stats.Targets[0].Matched)
	}
	if _, present := doc["email"]; !present {
		t.Fatalf("deep remove mutated the caller's nested map")
	}
}

func TestMaskRemove_PathDropsOnlyThatPath(t *testing.T) {
	profile := map[string]interface{}{"email": "p@example.com", "city": "Pune"}
	in := []Row{
		{"_id": "x1", "email": "top@example.com", "document": map[string]interface{}{"profile": profile}},
		{"_id": "x2", "document": map[string]interface{}{"items": []interface{}{
			map[string]interface{}{"email": "a@example.com"},
			map[string]interface{}{"email": "b@example.com"},
		}}},
	}
	out, stats := applyMaskRule(t, map[string]interface{}{
		"paths":     []interface{}{"document.profile.email", "document.items.email"},
		"mask_type": "remove",
	}, in)

	p := out[0]["document"].(map[string]interface{})["profile"].(map[string]interface{})
	if v, present := p["email"]; present {
		t.Errorf("path remove kept document.profile.email=%v", v)
	}
	if p["city"] != "Pune" {
		t.Errorf("path remove lost a sibling: %v", p)
	}
	if out[0]["email"] != "top@example.com" {
		t.Errorf("path remove touched the top-level email: %v", out[0]["email"])
	}
	for i, it := range out[1]["document"].(map[string]interface{})["items"].([]interface{}) {
		if v, present := it.(map[string]interface{})["email"]; present {
			t.Errorf("path remove kept items[%d].email=%v", i, v)
		}
	}
	if stats.Targets[0].Matched != 1 || stats.Targets[1].Matched != 2 {
		t.Errorf("matched %+v, want 1 and 2", stats.Targets)
	}
	if _, present := profile["email"]; !present {
		t.Fatalf("path remove mutated the caller's nested map")
	}
}

// The plaintext guard runs after every mask: a removed value is not a residual.
func TestMaskRemove_PassesThePlaintextGuard(t *testing.T) {
	raw := []map[string]interface{}{
		{"type": "mask_pii", "config": map[string]interface{}{"column": "country", "mask_type": "remove"}, "order": 1},
	}
	chain, _, err := NormalizeAndValidate(raw, "users", NormalizeModeExecution)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	in := []Row{{"id": 1, "country": "IN"}}
	snap, err := SnapshotPlaintext(in, chain)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	out, _ := applyMaskRule(t, chain[0].Config, in)
	residual, err := VerifyNoResidualPlaintext(snap, out)
	if err != nil || len(residual) != 0 {
		t.Fatalf("guard: residual=%v err=%v", residual, err)
	}
}
