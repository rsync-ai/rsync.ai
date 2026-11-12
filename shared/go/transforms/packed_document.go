package transforms

import (
	"sort"
	"strings"
)

// Rules on a packed MongoDB CDC row
//
// The CDC sink hands a MongoDB change event to the consumer chain packed as
// {_id, document:{...}}: the whole document is one column. The sink applies
// two rule types inside the document — mask_pii (column targets at any depth,
// path targets from the row root) and type_convert (the document's fields).
// Every other rule names a top-level column, and on a packed row that column
// is not there: a filter on `status` passed every row, a select on `email`
// emptied each row, and the run said nothing. Those rules are refused instead,
// by the sink (the batch goes to the DLQ) and by the API that saves them.
// MongoDB BATCH rows are the document's own fields, so every rule runs there.

// packedDocumentRuleTypes are the rule types the sink can apply to a packed
// MongoDB CDC row.
var packedDocumentRuleTypes = map[string]struct{}{
	"mask_pii":     {},
	"type_convert": {},
}

// RunsOnPackedDocument reports whether a rule of this type can run on a packed
// MongoDB CDC row.
func RunsOnPackedDocument(ruleType string) bool {
	_, ok := packedDocumentRuleTypes[normalizeType(ruleType)]
	return ok
}

// PackedDocumentRuleTypes returns, sorted, the rule types the sink can apply to
// a packed MongoDB CDC row: the only types a UI should offer for a consumer rule
// on such a pipeline (api-gateway GetPipelineTransforms, consumer_rule_types).
func PackedDocumentRuleTypes() []string {
	out := make([]string, 0, len(packedDocumentRuleTypes))
	for t := range packedDocumentRuleTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// RulesUnsupportedOnPackedDocument returns the distinct types, in chain order,
// of the enabled rules that cannot run on a packed MongoDB CDC row. Empty
// means the whole chain can.
func RulesUnsupportedOnPackedDocument(canonical []CanonicalTransform) []string {
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, t := range canonical {
		if !t.Enabled || RunsOnPackedDocument(t.Type) {
			continue
		}
		tt := normalizeType(t.Type)
		if tt == "" {
			tt = t.Type
		}
		if _, dup := seen[tt]; dup {
			continue
		}
		seen[tt] = struct{}{}
		out = append(out, tt)
	}
	return out
}

// IsMongoSourceType reports whether a source connector type is MongoDB-family:
// its CDC rows reach the consumer chain packed, its batch rows are the
// document's own fields.
func IsMongoSourceType(connectorType string) bool {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(connectorType)), "_", "-") {
	case "mongodb", "mongo", "mongodb-atlas", "mongodbatlas", "atlas":
		return true
	}
	return false
}
