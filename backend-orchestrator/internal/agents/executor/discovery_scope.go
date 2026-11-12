package executor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rsync-ai/backend-orchestrator/pkg/namespacefilter"
	"github.com/rsync-ai/backend-orchestrator/pkg/namespacemodel"
)

// A connection's Scope (namespace_filter_mode / namespace_filter_patterns, #31)
// narrows which databases or schemas its discovery shows. The server-level
// MySQL/MongoDB/ClickHouse connectors apply it themselves, so they never
// enumerate a database the user left out; every other connector (PostgreSQL
// schemas, SQL Server, …) knows nothing about it. discovery applies it here,
// once, for all of them — on top of a connector that already did, it is a no-op.

// scopeCategory is the warnings_objects category of the "matched nothing"
// warning, the same one the connectors use.
const scopeCategory = "scope_empty"

// scopeUnplacedCategory is the warnings_objects category of the warning for
// discovered tables that did not say which namespace they are in.
const scopeUnplacedCategory = "scope_unplaced"

// connectionScope returns the connection's Scope and whether it narrows
// anything. A connection that names the one namespace its discovery reads
// (pinnedNamespace) is pinned to it: its scope is ignored, exactly as the
// connector ignores it. A scope that does not parse is an error — never "all".
func connectionScope(connectorType string, config map[string]string) (namespacefilter.Filter, bool, error) {
	if pinnedNamespace(connectorType, config) != "" {
		return namespacefilter.Filter{}, false, nil
	}
	f, err := namespacefilter.Parse(config)
	if err != nil {
		return namespacefilter.Filter{}, false, err
	}
	return f, f.Active(), nil
}

// pinnedNamespace is the one namespace a connection's discovery reads, or ""
// when it may span several (namespace_model table_namespace): the configured
// database of a "database" source, or the dataset_id of a "dataset" one —
// BigQuery lists only that dataset's tables, so a Scope naming another dataset
// could only empty the list.
func pinnedNamespace(connectorType string, config map[string]string) string {
	switch namespacemodel.For(connectorType).TableNamespace {
	case "database":
		return configuredDatabase(config)
	case "dataset":
		return strings.TrimSpace(config["dataset_id"])
	}
	return ""
}

// scopeError is the fail-closed discovery error for a Scope that does not
// parse. The filter's messages are fixed text (no names, no patterns).
func scopeError(connectorType string, err error) error {
	msg := err.Error()
	if !errors.Is(err, namespacefilter.ErrInvalid) {
		msg = namespacefilter.ErrInvalid.Error() + ": " + msg
	}
	return &DiscoveryFailedError{Connector: connectorType, Message: msg}
}

// inScope reports whether a discovered table in namespace ns (its "schema"
// field: a schema, a database or a BigQuery dataset, per the source's
// namespace_model table_namespace) passes an active Scope. System names are
// the connectors' business — they already leave them out — so none are passed.
// A table that names no namespace cannot be placed: it is left out whatever
// the mode, failing closed as a Scope that does not parse does, and is reported
// as the connector's defect (unplacedWarning), never as the patterns matching
// nothing.
func inScope(f namespacefilter.Filter, ns string) bool {
	return strings.TrimSpace(ns) != "" && f.Allowed(ns, nil)
}

// scopeTables keeps the tables an active Scope allows (inScope).
func scopeTables(tables []TableMetadata, f namespacefilter.Filter) []TableMetadata {
	out := make([]TableMetadata, 0, len(tables))
	for _, t := range tables {
		if inScope(f, t.Schema) {
			out = append(out, t)
		}
	}
	return out
}

// unplacedTables counts the tables that name no namespace.
func unplacedTables(tables []TableMetadata) int {
	n := 0
	for _, t := range tables {
		if strings.TrimSpace(t.Schema) == "" {
			n++
		}
	}
	return n
}

// unplacedWarning says that n discovered tables were left out because they did
// not name their namespace, in the source's own word for it.
func unplacedWarning(connectorType string, n int) string {
	return fmt.Sprintf("%d discovered table(s) did not say which %s they are in, so the connection's Scope could not place them and left them out. The connector must name each table's %s.",
		n, tableNamespaceNoun(connectorType), tableNamespaceNoun(connectorType))
}

// tableNamespaceNoun is the source's word for a table's namespace: "schema",
// "database" or "dataset" (namespace_model table_namespace).
func tableNamespaceNoun(connectorType string) string {
	return namespacemodel.For(connectorType).TableNamespace
}

// applyScopeToTotals keeps totals honest after dropping `removed` discovered
// tables: they leave both counts, so a list the connector truncated stays
// truncated and a complete one stays complete.
func applyScopeToTotals(t discoveryTotals, removed int) discoveryTotals {
	t.Discovered -= removed
	t.Available -= removed
	if t.Available < t.Discovered {
		t.Available = t.Discovered
	}
	return t
}

// scopeEnvelope applies the connection's Scope to a raw discover_schema result
// in place: out-of-scope tables leave "tables", the counts follow, and a scope
// that leaves nothing adds a warning instead of failing. Tables that named no
// namespace get their own warning (unplacedWarning); "matched nothing" is added
// only when a table that did name one was there to match.
func scopeEnvelope(connectorType string, config map[string]string, result map[string]interface{}) error {
	f, active, err := connectionScope(connectorType, config)
	if err != nil {
		return scopeError(connectorType, err)
	}
	if !active || result == nil {
		return nil
	}
	raw, _ := result["tables"].([]interface{})
	kept := make([]interface{}, 0, len(raw))
	unplaced := 0
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		schema, _ := m["schema"].(string)
		if strings.TrimSpace(schema) == "" {
			unplaced++
		}
		if inScope(f, schema) {
			kept = append(kept, item)
		}
	}
	removed := len(raw) - len(kept)
	result["tables"] = kept
	if removed > 0 {
		if v, ok := result["total_tables_available"].(float64); ok {
			result["total_tables_available"] = maxFloat(v-float64(removed), float64(len(kept)))
		}
		if _, ok := result["total_tables_discovered"]; ok {
			result["total_tables_discovered"] = float64(len(kept))
		}
	}
	if unplaced > 0 {
		addWarning(result, scopeUnplacedCategory, unplacedWarning(connectorType, unplaced))
	}
	if len(kept) == 0 && len(raw) > unplaced {
		addScopeWarning(result)
	}
	return nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// addScopeWarning adds the "matched nothing" warning once, in both warning
// shapes the connectors emit.
func addScopeWarning(result map[string]interface{}) {
	addWarning(result, scopeCategory, namespacefilter.NoMatchWarning)
}

// addWarning adds a warning of this category once, in both warning shapes the
// connectors emit.
func addWarning(result map[string]interface{}, category, message string) {
	objs, _ := result["warnings_objects"].([]interface{})
	for _, o := range objs {
		if m, ok := o.(map[string]interface{}); ok && m["category"] == category {
			return
		}
	}
	result["warnings_objects"] = append(objs, map[string]interface{}{
		"category": category, "severity": "warning", "message": message,
	})
	msgs, _ := result["warnings_messages"].([]interface{})
	result["warnings_messages"] = append(msgs, message)
}
