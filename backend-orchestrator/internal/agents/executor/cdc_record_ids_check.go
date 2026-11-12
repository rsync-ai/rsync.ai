package executor

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// A CDC run into an API destination that addresses each record by the id its
// API assigned (get_capabilities "record_ids", connector.py.j2) fails at start,
// before the source is read (KI-API-DEST-CDC-WRITES-BY-SOURCE-ID). Only the batch
// lane keeps the source-row → record-id map (kafka-mcp-sink api_dest_rows.go); the
// CDC lane wrote each change by the source row's own id, creating the record again
// or changing whichever API record carried that id. The gateway refuses to create
// such a pipeline (api-gateway preflight_pipeline.go); this catches one it never
// saw: created before that gate, switched to CDC later, or into a connector
// generated before its metadata.json declared record_ids. The CDC sink restart
// (handlers/cdc_sink.go) refuses with the same message.

// cdcRecordIDsCheck is "" when the run may start: the destination does not take
// record ids, writes CDC as an append-only history, or gives no answer (as with
// no MCP manager to ask, capability_gate.go).
func (a *Agent) cdcRecordIDsCheck(ctx context.Context, task ExecutorTask) string {
	if a.mcpManager == nil || task.Destination == nil || strings.TrimSpace(task.Destination.Type) == "" ||
		CDCAppendOnlyDestination(task.Destination.Config) {
		return ""
	}
	requested := strings.TrimSpace(task.Destination.Version)
	if requested == "" {
		requested = task.Destination.Config["connector_version"]
	}
	version := concreteVersionOrRequested(a.mcpManager, task.Destination.Type, requested)
	caps, ok := a.destCapabilities(ctx, task.Destination.Type, version, task.Destination.Config)
	if !ok {
		log.WithFields(log.Fields{"pipeline_id": task.PipelineID, "destination": task.Destination.Type}).
			Warn("CDC record-ids check skipped: get_capabilities gave no answer")
		return ""
	}
	return CDCRecordIDsRefusal(caps, task.Destination.Type, task.Destination.Config)
}

// CDCRecordIDsRefusal is the error a CDC run into destType fails with, or "" when
// its capabilities do not say record_ids or destCfg writes CDC as append-only
// history (every change a new record, so none is addressed by a source id).
func CDCRecordIDsRefusal(caps map[string]interface{}, destType string, destCfg map[string]string) string {
	if !capabilityFlag(caps, "record_ids") || CDCAppendOnlyDestination(destCfg) {
		return ""
	}
	return fmt.Sprintf(
		"%s addresses each record by the id its API assigned, and only a batch run keeps the map "+
			"from a source row to that id. A CDC run would create the record again, or change "+
			"whichever %s record carries the source row's id. Run this pipeline as a batch sync, "+
			"or set cdc_write_mode=append on the destination connection to add each change as a new record.",
		destType, destType)
}

// CDCAppendOnlyDestination reports whether the destination's cdc_write_mode is one
// the kafka-mcp-sink writes as append-only history (main.go cdcAppendMode).
// TestCDCAppendOnlyDestination_MatchesTheSink holds the two lists equal.
func CDCAppendOnlyDestination(cfg map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg["cdc_write_mode"])) {
	case "append", "append_only", "append-only", "history", "insert":
		return true
	}
	return false
}

// capabilityFlag reads a get_capabilities flag at the top level or under
// "capabilities", as the sink does (probeDestCapabilities, toBool).
func capabilityFlag(caps map[string]interface{}, name string) bool {
	truthy := func(v interface{}) bool {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			s := strings.ToLower(strings.TrimSpace(t))
			return s == "true" || s == "1" || s == "yes" || s == "y"
		}
		return false
	}
	if truthy(caps[name]) {
		return true
	}
	nested, ok := caps["capabilities"].(map[string]interface{})
	return ok && truthy(nested[name])
}
