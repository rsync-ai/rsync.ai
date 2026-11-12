package executor

import (
	"fmt"
	"regexp"
	"strings"
)

// missingPathParamRe matches a generated connector refusing to build a URL with
// a blank path parameter (connector.py.j2 _make_request_v2). The capture must
// look like a setting key; anything else could be a value and is not taken.
var missingPathParamRe = regexp.MustCompile(`Missing required path parameter '([A-Za-z_][A-Za-z0-9_.-]{0,63})'`)

// missingConfigField returns the connection setting a raw export error says is
// missing, or "". Read from the RAW error: llmscrub masks every quoted string,
// the setting's name with it.
func missingConfigField(raw string) string {
	if m := missingPathParamRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return ""
}

// noteMissingConfig appends the setting a failed table export names, once per
// setting. The run's wait step shows it and opens the connection to set it.
func noteMissingConfig(acc []map[string]interface{}, table, raw string) []map[string]interface{} {
	field := missingConfigField(raw)
	if field == "" {
		return acc
	}
	for _, m := range acc {
		if m["field"] == field {
			return acc
		}
	}
	return append(acc, map[string]interface{}{"table": table, "field": field, "direction": "source"})
}

// exportFailureResponse is the run's failure when a table export failed. Any
// per-table export error fails the whole pipeline: partial syncs must never
// report success (audit finding T2-4).
func exportFailureResponse(task ExecutorTask, totalRows int64, lastExportError string, missingConfig []map[string]interface{}) ExecutorResponse {
	if strings.TrimSpace(lastExportError) == "" {
		lastExportError = "export failed (no details)"
	}
	resp := ExecutorResponse{
		TaskID:     task.TaskID,
		PipelineID: task.PipelineID,
		Status:     "failed",
		Error:      fmt.Sprintf("batch export failed (partial sync, %d rows landed before error): %s", totalRows, lastExportError),
	}
	if len(missingConfig) > 0 {
		resp.Result = map[string]interface{}{"missing_config": missingConfig}
	}
	return resp
}
