package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rsync-ai/shared/transforms"
	"github.com/segmentio/kafka-go"
)

// KI-MONGO-CDC-MASK-SILENT-NOOP: a MongoDB change event lands as a PACKED row,
// {_id, document}, with the whole source document nested under "document". A
// mask rule names a field ("email") the way it would name a relational column,
// so on the packed row it matched no top-level key and the value reached the
// destination in plaintext. The bug class: a mask written for a flat row meets
// a row whose fields are nested. These tests drive the real decode path and the
// real consumer chain, and assert on the serialized row the destination gets.

const (
	mongoMaskTopEmail    = "asha.rao@example.com"
	mongoMaskNestedEmail = "asha.alt@example.com"
	mongoMaskItemEmail   = "billing@example.com"
	mongoMaskPhone       = "+44 7700 900123"
)

func mongoMaskDocument() string {
	return `{"_id":{"$oid":"6512a0c4e1b2c3d4e5f60718"},` +
		`"name":"Asha Rao","email":"` + mongoMaskTopEmail + `","status":"active",` +
		`"contact":{"Email":"` + mongoMaskNestedEmail + `","phone":"` + mongoMaskPhone + `"},` +
		`"cards":[{"last4":"4242","email":"` + mongoMaskItemEmail + `"}],` +
		`"opened_at":{"$date":1696000000000}}`
}

// mongoMaskEvent parses a Debezium MongoDB change event exactly as the sink
// does: before/after arrive as JSON strings, the key as Extended JSON.
func mongoMaskEvent(t *testing.T, op string) *SinkMessage {
	t.Helper()
	cfg := &WorkerConfig{PipelineID: "p1", SinkMode: "cdc", DestinationConnector: "gcs"}
	payload := map[string]interface{}{
		"op":     op,
		"source": map[string]interface{}{"connector": "mongodb", "db": "fintech_app", "collection": "customers"},
	}
	if op == "d" {
		payload["before"] = mongoMaskDocument()
	} else {
		payload["after"] = mongoMaskDocument()
	}
	msg := kafka.Message{
		Topic:  "rsync.fintech_app.customers",
		Offset: 1,
		Key:    []byte(`{"id":"{\"$oid\":\"6512a0c4e1b2c3d4e5f60718\"}"}`),
	}
	sm, err := parseCDCMessage(cfg, msg, payload, map[string]interface{}{})
	if err != nil {
		t.Fatalf("parseCDCMessage(op=%s): %v", op, err)
	}
	if len(sm.Data) != 1 {
		t.Fatalf("op=%s decoded %d rows, want 1", op, len(sm.Data))
	}
	return sm
}

// mongoMaskChain is the chain the sink builds from transform_definitions rows,
// in the shape the NL gate and the transforms API store (operation=mask).
func mongoMaskChain(t *testing.T, configs ...map[string]any) []transforms.CanonicalTransform {
	t.Helper()
	raw := make([]map[string]any, 0, len(configs))
	for i, c := range configs {
		raw = append(raw, map[string]any{
			"id":               "00000000-0000-0000-0000-00000000000" + string(rune('1'+i)),
			"transform_order":  i + 1,
			"enabled":          true,
			"transform_config": c,
		})
	}
	canonical, _, err := transforms.NormalizeAndValidate(raw, "customers", transforms.NormalizeModeCDC)
	if err != nil {
		t.Fatalf("chain did not validate: %v", err)
	}
	return canonical
}

func emailMask() map[string]any {
	return map[string]any{"operation": "mask", "column": "email", "mask_type": "hash", "hash_function": "sha256"}
}

func mongoMaskSerialized(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func packedDocument(t *testing.T, row map[string]interface{}) map[string]interface{} {
	t.Helper()
	doc, ok := row["document"].(map[string]interface{})
	if !ok {
		t.Fatalf("document is %T, want a map", row["document"])
	}
	return doc
}

func TestMongoCDCMask_MasksTheFieldInsideTheDocument(t *testing.T) {
	for _, op := range []string{"c", "r", "u"} {
		t.Run("op="+op, func(t *testing.T) {
			sm := mongoMaskEvent(t, op)
			if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, emailMask())); err != nil {
				t.Fatalf("apply: %v", err)
			}

			out := mongoMaskSerialized(t, sm.Data)
			for _, plain := range []string{mongoMaskTopEmail, mongoMaskNestedEmail, mongoMaskItemEmail} {
				if strings.Contains(out, plain) {
					t.Errorf("plaintext %q reached the destination row: %s", plain, out)
				}
			}

			doc := packedDocument(t, sm.Data[0])
			if got, _ := doc["email"].(string); !strings.HasPrefix(got, "sha256:") {
				t.Errorf("document.email = %v, want a sha256: hash", doc["email"])
			}
			contact, _ := doc["contact"].(map[string]interface{})
			if got, _ := contact["Email"].(string); !strings.HasPrefix(got, "sha256:") {
				t.Errorf("document.contact.Email = %v, want a sha256: hash (field names match in any case)", contact["Email"])
			}
			// What the rule did not name is left alone.
			if doc["name"] != "Asha Rao" || doc["status"] != "active" || contact["phone"] != mongoMaskPhone {
				t.Errorf("unmasked fields changed: name=%v status=%v phone=%v", doc["name"], doc["status"], contact["phone"])
			}
			if sm.Data[0]["_id"] != "6512a0c4e1b2c3d4e5f60718" {
				t.Errorf("_id = %v, the packed key must not change", sm.Data[0]["_id"])
			}

			// The CDC envelope the writers read must carry the masked row too.
			if strings.Contains(mongoMaskSerialized(t, sm.After), mongoMaskTopEmail) {
				t.Errorf("sm.After still holds the plaintext email")
			}
		})
	}
}

// A delete with a pre-image is written in full (append mode writes the before
// image as the tombstone), so it is masked like any other row.
func TestMongoCDCMask_MasksADeletesBeforeImage(t *testing.T) {
	sm := mongoMaskEvent(t, "d")
	if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, emailMask())); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for name, v := range map[string]interface{}{"data": sm.Data, "before": sm.Before} {
		if s := mongoMaskSerialized(t, v); strings.Contains(s, mongoMaskTopEmail) || strings.Contains(s, mongoMaskNestedEmail) {
			t.Errorf("delete %s still holds a plaintext email: %s", name, s)
		}
	}
	if sm.CDCOp != "d" || sm.PK["_id"] != "6512a0c4e1b2c3d4e5f60718" {
		t.Errorf("delete lost its op or key: op=%q pk=%v", sm.CDCOp, sm.PK)
	}
}

// Every way a rule can name its fields: a list, a table-qualified name, and the
// other mask types, including remove, which drops the key.
func TestMongoCDCMask_EveryWayToNameAndMask(t *testing.T) {
	cases := []struct {
		name  string
		cfg   map[string]any
		check func(t *testing.T, doc map[string]interface{})
	}{
		{"columns list", map[string]any{"operation": "mask_pii", "columns": []any{"email", "phone"}, "mask_type": "hash"},
			func(t *testing.T, doc map[string]interface{}) {
				contact := doc["contact"].(map[string]interface{})
				if p, _ := contact["phone"].(string); !strings.HasPrefix(p, "sha256:") {
					t.Errorf("contact.phone = %v, want a hash", contact["phone"])
				}
			}},
		{"table-qualified column", map[string]any{"operation": "mask", "column": "customers.email", "mask_type": "redact"},
			func(t *testing.T, doc map[string]interface{}) {
				if doc["email"] != "***" {
					t.Errorf("document.email = %v, want ***", doc["email"])
				}
			}},
		{"hash card", map[string]any{"operation": "hash", "column": "email"},
			func(t *testing.T, doc map[string]interface{}) {
				if e, _ := doc["email"].(string); !strings.HasPrefix(e, "sha256:") {
					t.Errorf("document.email = %v, want a hash", doc["email"])
				}
			}},
		{"remove", map[string]any{"operation": "mask_pii", "column": "email", "mask_type": "remove"},
			func(t *testing.T, doc map[string]interface{}) {
				if _, still := doc["email"]; still {
					t.Errorf("document.email still present: remove drops the key")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := mongoMaskEvent(t, "c")
			if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, tc.cfg)); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if out := mongoMaskSerialized(t, sm.Data); strings.Contains(out, mongoMaskTopEmail) {
				t.Errorf("plaintext email reached the destination row: %s", out)
			}
			tc.check(t, packedDocument(t, sm.Data[0]))
		})
	}
}

// The packed table has a fixed shape, _id + document. Reconciling column types
// for a mask used to add the masked field as a column of its own, so the
// destination grew an empty "email" column next to the document.
func TestMongoCDCMask_KeepsThePackedShape(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	before := map[string]string{}
	for k, v := range sm.ColumnTypes {
		before[k] = v
	}
	chain := mongoMaskChain(t, emailMask(),
		map[string]any{"operation": "type_convert", "column": "status", "to": "string"})
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(sm.ColumnTypes) != len(before) {
		t.Errorf("column types changed from %v to %v; the packed table is _id + document only", before, sm.ColumnTypes)
	}
	for k, v := range before {
		if sm.ColumnTypes[k] != v {
			t.Errorf("column %s type %q -> %q", k, v, sm.ColumnTypes[k])
		}
	}
	if keys := len(sm.Data[0]); keys != 2 {
		t.Errorf("packed row has %d keys, want 2 (_id, document): %v", keys, sm.Data[0])
	}
}

// The chain is retried in place on a write failure; the second pass must not
// hash the hash.
func TestMongoCDCMask_ARetryDoesNotMaskTwice(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	chain := mongoMaskChain(t, emailMask())
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first := packedDocument(t, sm.Data[0])["email"]
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if again := packedDocument(t, sm.Data[0])["email"]; again != first {
		t.Errorf("retry re-masked: %v -> %v", first, again)
	}
}

// The deep switch belongs to the packed row only. A relational row keeps its
// top-level, exact-name mask, so a JSON column that happens to hold a key named
// like the masked column is not rewritten behind the user's back. When it holds
// one, the mask guard stops the event instead of letting that copy through in
// plaintext: a mask rule says the value must not leave, and the error names the
// target and how to reach it, never the value.
func TestMongoCDCMask_RelationalRowsKeepTopLevelMatching(t *testing.T) {
	relational := func(row map[string]interface{}) *SinkMessage {
		sm := &SinkMessage{
			Table: "customers", IsCDC: true, CDCOp: "c",
			Data: []map[string]interface{}{row},
			PK:   map[string]interface{}{"id": 7}, KeyFields: []string{"id"}, RowCount: 1,
		}
		sm.After = sm.Data[0]
		return sm
	}

	sm := relational(map[string]interface{}{
		"id": 7, "email": mongoMaskTopEmail,
		"profile": map[string]interface{}{"tier": "gold"},
	})
	if err := applyCDCTransformChain(context.Background(), nil, sm, mongoMaskChain(t, emailMask())); err != nil {
		t.Fatalf("apply: %v", err)
	}
	row := sm.Data[0]
	if e, _ := row["email"].(string); !strings.HasPrefix(e, "sha256:") {
		t.Errorf("email = %v, want a hash", row["email"])
	}
	if p := row["profile"].(map[string]interface{}); p["tier"] != "gold" {
		t.Errorf("profile.tier = %v: a relational mask leaves other nested values alone", p["tier"])
	}
	if sm.ColumnTypes["email"] != "string" {
		t.Errorf("relational column types still reconcile: email = %q, want string", sm.ColumnTypes["email"])
	}

	nested := relational(map[string]interface{}{
		"id": 7, "email": mongoMaskTopEmail,
		"profile": map[string]interface{}{"email": mongoMaskNestedEmail},
	})
	err := applyCDCTransformChain(context.Background(), nil, nested, mongoMaskChain(t, emailMask()))
	if !errors.Is(err, transforms.ErrResidualPlaintext) {
		t.Fatalf("a nested copy under a top-level mask: err = %v, want ErrResidualPlaintext", err)
	}
	if strings.Contains(err.Error(), "example.com") {
		t.Fatalf("the error carries a row value: %v", err)
	}
	if nested.consumerTransformsApplied {
		t.Fatal("a refused event must not be marked as transformed")
	}
}

// The packed row's own keys are the sink's, not the user's. A collection whose
// documents have a field called "document" (a KYC record, say) gets that field
// masked; the wrapper around the whole document is left alone, so the row is
// still a document and not one hash of everything.
func TestMongoCDCMask_AFieldNamedDocumentIsNotTheWrapper(t *testing.T) {
	cfg := &WorkerConfig{PipelineID: "p1", SinkMode: "cdc", DestinationConnector: "postgresql"}
	payload := map[string]interface{}{
		"op":     "c",
		"source": map[string]interface{}{"connector": "mongodb", "db": "fintech_app", "collection": "kyc"},
		"after":  `{"_id":"k1","customer":"Asha Rao","document":{"type":"passport","number":"P1234567"}}`,
	}
	sm, err := parseCDCMessage(cfg, kafka.Message{Topic: "rsync.fintech_app.kyc", Offset: 1, Key: []byte(`{"id":"k1"}`)}, payload, map[string]interface{}{})
	if err != nil {
		t.Fatalf("parseCDCMessage: %v", err)
	}
	chain := mongoMaskChain(t, map[string]any{"operation": "mask", "column": "document", "mask_type": "redact"})
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	doc := packedDocument(t, sm.Data[0])
	if doc["document"] != "***" {
		t.Errorf("document.document = %v, want ***", doc["document"])
	}
	if doc["customer"] != "Asha Rao" || sm.Data[0]["_id"] != "k1" {
		t.Errorf("the rest of the row changed: %v", sm.Data[0])
	}
	if strings.Contains(mongoMaskSerialized(t, sm.Data), "P1234567") {
		t.Errorf("passport number reached the destination row")
	}
}

// An explicit path runs from the packed row's root, as the engine documents it,
// and touches only the field it names.
func TestMongoCDCMask_AnExplicitPathFromTheRowRoot(t *testing.T) {
	sm := mongoMaskEvent(t, "c")
	chain := mongoMaskChain(t, map[string]any{"operation": "mask_pii", "path": "document.contact.phone", "mask_type": "redact"})
	if err := applyCDCTransformChain(context.Background(), nil, sm, chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	doc := packedDocument(t, sm.Data[0])
	if contact := doc["contact"].(map[string]interface{}); contact["phone"] != "***" {
		t.Errorf("document.contact.phone = %v, want ***", contact["phone"])
	}
	if doc["email"] != mongoMaskTopEmail {
		t.Errorf("document.email = %v: a path rule masks only its path", doc["email"])
	}
}
