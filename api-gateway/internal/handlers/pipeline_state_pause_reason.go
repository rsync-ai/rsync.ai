package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// isTableSelectionPark reports whether a wait asks the user to pick source
// tables: the batch path's table-selection types, or a DAG node wait
// (node_input) whose details carry the executor's action_needed.
func isTableSelectionPark(blockingType string, details map[string]interface{}) bool {
	if isTableSelectionType(blockingType) {
		return true
	}
	if blockingType != "node_input" || details == nil {
		return false
	}
	action, _ := details["action_needed"].(string)
	return action == "table_selection"
}

// describeTableSelectionPark is the card text for a table-selection wait, or ""
// to keep the stored one. It reads the pipeline's NL request only when there is
// a table list to check the named tables against.
func describeTableSelectionPark(ctx context.Context, database *sql.DB, pipelineID string, details map[string]interface{}) string {
	if database == nil || len(parkTableRefs(details)) == 0 {
		return tableSelectionReason("", details)
	}
	var nlRequest sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT natural_language_request FROM pipelines WHERE id = $1`, pipelineID,
	).Scan(&nlRequest); err != nil {
		return tableSelectionReason("", details)
	}
	return tableSelectionReason(nlRequest.String, details)
}

// tableSelectionReason says why a run paused to pick tables. When the NL
// request named tables it checks them against the wait's table list, so the
// card says "Couldn't find `events` in the source" rather than the executor's
// generic reason (the executor never sees the request). Otherwise it is the
// executor's reason, or "" when there is none.
func tableSelectionReason(nlRequest string, details map[string]interface{}) string {
	reason, _ := details["reason"].(string)
	reason = strings.TrimSpace(reason)
	known := parkTableRefs(details)
	requested := parseTableIntent(nlRequest)
	if len(known) == 0 || len(requested) == 0 {
		return reason
	}

	source, _ := details["source_type"].(string)
	if strings.TrimSpace(source) == "" {
		source = "the source"
	}
	truncated, _ := details["tables_truncated"].(bool)
	total := len(known)
	if n, ok := details["total_tables_available"].(float64); ok && int(n) > total {
		total = int(n)
	}

	qualified, missing, ambiguous := qualifyNamedTables(known, requested)
	var parts []string
	if len(missing) > 0 {
		if truncated {
			// The list is cut: a name missing from it may exist past the cut.
			parts = append(parts, fmt.Sprintf("%s isn't among the %d listed — type its name to select it.", backtickList(missing), len(known)))
		} else {
			parts = append(parts, "Couldn't find "+backtickList(missing)+" in the source.")
		}
	}
	names := make([]string, 0, len(ambiguous))
	for n := range ambiguous {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("`%s` exists in more than one schema (%s).", n, backtickList(ambiguous[n])))
	}
	if len(parts) == 0 && len(qualified) > 0 {
		parts = append(parts, "Confirm the tables you named: "+backtickList(qualified)+".")
	}

	tail := fmt.Sprintf("Found %d tables/resources in %s. Select what to sync.", len(known), source)
	if truncated {
		tail = fmt.Sprintf("Listed %d of %d tables/resources in %s. Select what to sync.", len(known), total, source)
	}
	return strings.Join(append(parts, tail), " ")
}

// parkTableRefs reads the wait's available_tables ({name, schema, …}) as the
// table list qualifyNamedTables matches names against.
func parkTableRefs(details map[string]interface{}) []schemaTableRef {
	arr, _ := details["available_tables"].([]interface{})
	refs := make([]schemaTableRef, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			name, _ = m["table"].(string)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		schema, _ := m["schema"].(string)
		refs = append(refs, schemaTableRef{Name: name, Schema: schema})
	}
	return refs
}

// isTransformsReviewPark reports a table-selection wait raised only to show the
// chat's transform step for tables the message already named: the flag plus a
// non-empty table list, the same pair the worker writes together.
func isTransformsReviewPark(details map[string]interface{}) bool {
	if review, _ := details["review_transforms_only"].(bool); !review {
		return false
	}
	switch tables := details["preselected_tables"].(type) {
	case []interface{}:
		return len(tables) > 0
	case []string:
		return len(tables) > 0
	}
	return false
}
