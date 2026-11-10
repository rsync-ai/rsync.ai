package main

import (
	"context"
	"testing"

	"github.com/rsync-ai/shared/transforms"
)

// KI-CDC-FILTER-KEEPS-ROW-THAT-LEFT-IT: a filter decides which rows the
// destination holds. An update that moves a row OUT of that set used to be
// dropped like an insert the filter rejected, so the destination kept the row's
// last matching version forever. It has to reach the destination as a delete.

func updateMessage(before, after map[string]interface{}) *SinkMessage {
	sm := &SinkMessage{
		Table:     "customers",
		IsCDC:     true,
		CDCOp:     "u",
		Before:    before,
		After:     after,
		PK:        map[string]interface{}{"id": 7},
		KeyFields: []string{"id"},
		RowCount:  1,
	}
	if after != nil {
		sm.Data = []map[string]interface{}{after}
	}
	return sm
}

func TestCDCUpdateLeavingTheFilter_BecomesADelete(t *testing.T) {
	sm := updateMessage(
		map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "active"},
		map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "inactive"},
	)

	if err := applyCDCTransformChain(context.Background(), nil, sm, deletePipelineChain(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if sm.CDCOp != "d" {
		t.Fatalf("op = %q, want d: the row left the filter, so the destination still holds a copy it must remove", sm.CDCOp)
	}
	if sm.After != nil {
		t.Errorf("a delete carries no after image, got %v", sm.After)
	}
	if len(sm.Data) != 1 || sm.Before == nil {
		t.Fatalf("the delete lost its row (data=%d, before=%v); append mode writes the tombstone from it", len(sm.Data), sm.Before)
	}
	// The tombstone is a row the destination sees, so it is shaped like one.
	if _, raw := sm.Before["email"]; raw {
		t.Errorf("tombstone still carries the source column `email`: %v", sm.Before)
	}
	if masked := sm.Before["contact_email"]; masked == nil || masked == "seven@example.com" {
		t.Errorf("tombstone's email was not masked: %v", masked)
	}
	if sm.PK["id"] != 7 || len(sm.KeyFields) != 1 || sm.KeyFields[0] != "id" {
		t.Errorf("the key an upsert destination deletes by was lost: pk=%v keys=%v", sm.PK, sm.KeyFields)
	}
}

// Without a before image (REPLICA IDENTITY not FULL, MongoDB without
// pre-images) there is no way to tell whether the destination holds the row,
// so the update is sent as a delete: deleting a row that is not there is a
// no-op, keeping one that should be gone is not.
func TestCDCUpdateLeavingTheFilter_NoBeforeImage_StillBecomesADelete(t *testing.T) {
	sm := updateMessage(nil, map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "inactive"})

	if err := applyCDCTransformChain(context.Background(), nil, sm, deletePipelineChain(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sm.CDCOp != "d" {
		t.Fatalf("op = %q, want d", sm.CDCOp)
	}
	if sm.Before == nil || sm.Before["id"] != 7 {
		t.Errorf("tombstone row missing: %v", sm.Before)
	}
}

// The control: an update whose row was ALREADY outside the filter has nothing at
// the destination to remove. Turning it into a delete would cost every such
// update a single-event write and, in append mode, record a tombstone for a row
// the destination never held.
func TestCDCUpdateOfARowAlreadyOutsideTheFilter_IsStillSkipped(t *testing.T) {
	sm := updateMessage(
		map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "inactive"},
		map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "closed"},
	)

	if err := applyCDCTransformChain(context.Background(), nil, sm, deletePipelineChain(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sm.CDCOp != "u" {
		t.Fatalf("op = %q, want u: the row was never in the filtered set", sm.CDCOp)
	}
	if sm.After != nil || len(sm.Data) != 0 {
		t.Errorf("a row outside the filter must not be written: after=%v data=%v", sm.After, sm.Data)
	}
}

func TestCDCUpdateStayingInTheFilter_StaysAnUpsert(t *testing.T) {
	sm := updateMessage(
		map[string]interface{}{"id": 7, "email": "seven@example.com", "status": "active"},
		map[string]interface{}{"id": 7, "email": "new@example.com", "status": "active"},
	)

	if err := applyCDCTransformChain(context.Background(), nil, sm, deletePipelineChain(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sm.CDCOp != "u" {
		t.Fatalf("op = %q, want u", sm.CDCOp)
	}
	if sm.After == nil || sm.After["contact_email"] == nil || sm.After["contact_email"] == "new@example.com" {
		t.Errorf("update was not written masked and renamed: %v", sm.After)
	}
}

// An insert the filter rejects never reached the destination, so it stays a
// skip - only an update can take a row away.
func TestCDCInsertRejectedByTheFilter_IsStillSkipped(t *testing.T) {
	after := map[string]interface{}{"id": 8, "email": "eight@example.com", "status": "inactive"}
	sm := &SinkMessage{Table: "customers", IsCDC: true, CDCOp: "c", After: after,
		Data: []map[string]interface{}{after}, PK: map[string]interface{}{"id": 8}, KeyFields: []string{"id"}}

	if err := applyCDCTransformChain(context.Background(), nil, sm, deletePipelineChain(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sm.CDCOp != "c" || sm.After != nil || len(sm.Data) != 0 {
		t.Errorf("rejected insert was not skipped: op=%q after=%v data=%v", sm.CDCOp, sm.After, sm.Data)
	}
}

// The single-event writer retries the SAME message in place after a transient
// destination failure, and every attempt ran the chain again: a hashed value was
// hashed a second time and the retry wrote sha256(sha256(x)).
func TestCDCTransformChain_ASecondRunOnTheSameMessageChangesNothing(t *testing.T) {
	raw := []map[string]any{
		{"type": "mask_pii", "config": map[string]any{"column": "email", "mask_type": "hash"}, "order": 1},
	}
	chain, _, err := transforms.NormalizeAndValidate(raw, "customers", transforms.NormalizeModeCDC)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	after := map[string]interface{}{"id": 9, "email": "nine@example.com"}
	sm := &SinkMessage{Table: "customers", IsCDC: true, CDCOp: "c", After: after, Data: []map[string]interface{}{after}}

	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	once := sm.Data[0]["email"]
	if once == "nine@example.com" {
		t.Fatalf("control is inert: the first run did not mask")
	}
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if twice := sm.Data[0]["email"]; twice != once {
		t.Errorf("the retry re-masked an already-masked value: %v -> %v", once, twice)
	}
}
