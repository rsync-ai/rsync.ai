package workflows

import (
	"fmt"
	"strings"
)

// connectionFixWaitDetails adds the UX hints the frontend's connection
// configurator reads to an executor-recovery wait.
//
// When the executor named the setting it could not find (its Result's
// missing_config, from agents/executor/missing_config.go), the wait lists that
// connection alone under missing_connections, with its ID, type and the
// setting, so the dialog can say "set list_id on this connection" and resume
// with it. Without one, the wait keeps the old shape (both connections offered).
func connectionFixWaitDetails(waitDetails map[string]interface{}, state *WorkflowState) map[string]interface{} {
	if waitDetails == nil {
		waitDetails = map[string]interface{}{}
	}
	waitDetails["request_type"] = "connection_config"
	waitDetails["action_needed"] = "connection_config"
	waitDetails["button_text"] = "Configure connections"
	waitDetails["source_connection_id"] = state.SourceConnectionID
	waitDetails["destination_connection_id"] = state.DestinationConnectionID

	// A run started from Run has no cached intent, so the dialog read
	// "Source: Unknown". The run's resolved type, then its pinned connector,
	// name it first.
	if _, ok := waitDetails["source_type"]; !ok {
		if v := connectorTypeHint(state.SourceConnectorType, state.SourceConnectorSnapshot, state.CachedIntent, "source_type"); v != "" {
			waitDetails["source_type"] = v
		}
	}
	if _, ok := waitDetails["destination_type"]; !ok {
		if v := connectorTypeHint(state.DestinationConnectorType, state.DestinationConnectorSnapshot, state.CachedIntent, "destination_type"); v != "" {
			waitDetails["destination_type"] = v
		}
	}

	if missing := missingConnectionsFromConfig(waitDetails["missing_config"], waitDetails, state); len(missing) > 0 {
		waitDetails["missing_connections"] = missing
	}
	return waitDetails
}

func connectorTypeHint(resolved string, snap *ConnectorSnapshot, intent map[string]interface{}, key string) string {
	if v := strings.TrimSpace(resolved); v != "" {
		return v
	}
	if snap != nil {
		if v := strings.TrimSpace(snap.Type); v != "" {
			return v
		}
	}
	if intent == nil {
		return ""
	}
	if v, ok := intent[key].(string); ok && v != "" {
		return v
	}
	if nested, ok := intent["intent"].(map[string]interface{}); ok {
		if v, ok := nested[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// missingConnectionsFromConfig groups the executor's missing settings by
// connection, one entry per direction, each setting once. raw is []interface{}
// after JSON, or []map[string]interface{} when built in-process.
func missingConnectionsFromConfig(raw interface{}, waitDetails map[string]interface{}, state *WorkflowState) []interface{} {
	var items []map[string]interface{}
	switch v := raw.(type) {
	case []interface{}:
		for _, it := range v {
			if m, ok := it.(map[string]interface{}); ok {
				items = append(items, m)
			}
		}
	case []map[string]interface{}:
		items = v
	}

	var out []interface{}
	byDirection := map[string]map[string]interface{}{}
	for _, it := range items {
		field := strings.TrimSpace(fmt.Sprint(it["field"]))
		if field == "" || field == "<nil>" {
			continue
		}
		direction, _ := it["direction"].(string)
		if direction != "destination" {
			direction = "source"
		}
		entry, ok := byDirection[direction]
		if !ok {
			connID, typ := state.SourceConnectionID, waitDetails["source_type"]
			if direction == "destination" {
				connID, typ = state.DestinationConnectionID, waitDetails["destination_type"]
			}
			ct, _ := typ.(string)
			entry = map[string]interface{}{
				"direction":      direction,
				"connection_id":  connID,
				"connector_type": ct,
				"missing_fields": []interface{}{},
			}
			byDirection[direction] = entry
			out = append(out, entry)
		}
		fields := entry["missing_fields"].([]interface{})
		seen := false
		for _, f := range fields {
			if f == field {
				seen = true
				break
			}
		}
		if !seen {
			entry["missing_fields"] = append(fields, field)
		}
	}
	return out
}

// resumedConnectionIDs applies a connections-configured signal. A blank ID in
// the signal means "unchanged": a Resume that sent none used to blank the IDs
// the run had, so the re-validation failed on a connection the user never
// touched.
func resumedConnectionIDs(curSrc, curDst string, p ConnectionsConfiguredPayload) (string, string) {
	src, dst := curSrc, curDst
	if v := strings.TrimSpace(p.SourceConnectionID); v != "" {
		src = v
	}
	if v := strings.TrimSpace(p.DestinationConnectionID); v != "" {
		dst = v
	}
	return src, dst
}
