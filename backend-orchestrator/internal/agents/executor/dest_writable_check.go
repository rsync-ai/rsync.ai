package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
	log "github.com/sirupsen/logrus"
)

// destWritableCheckTimeout bounds the get_capabilities call made before the
// sink starts. The check is advisory when the destination does not answer, so
// a slow connector must not hold the run's start for the full retry budget.
const destWritableCheckTimeout = 20 * time.Second

// destWritableCheck fails a run, before the sink starts and before the source is
// read, when the destination says it cannot take writes to the resource the run
// targets. A REST/GraphQL API names the resources a load can create records in
// (get_capabilities "writable_resources", connector.py.j2); a run aimed anywhere
// else had every row refused at write time (P5 live, 2026-10-06: "products" on a
// ClickUp connection). The gateway no longer stops these runs on SINK_NO_DDL
// (assessment.go sinkSupportsAutoCreate), so this is where a wrong target is caught.
//
// It returns "" — the run goes ahead — when there is nothing to check or the
// destination gives no answer: a connector that does not list writable resources
// (every database) is never blocked by this check.
func (a *Agent) destWritableCheck(ctx context.Context, destType, destVersion string, destCfg map[string]string, tables []string) string {
	targets := destWriteTargets(destCfg["table"], tables)
	if len(targets) == 0 || strings.TrimSpace(destType) == "" {
		return ""
	}
	caps, ok := a.destCapabilities(ctx, destType, destVersion, destCfg)
	if !ok {
		log.WithFields(log.Fields{"destination": destType, "targets": targets}).
			Warn("destination writable check skipped: get_capabilities gave no answer")
		return ""
	}
	return destResourceNotWritable(caps, targets, destType)
}

// destCapabilities is the destination's get_capabilities answer, bounded by
// destWritableCheckTimeout; ok is false when it gave none.
func (a *Agent) destCapabilities(ctx context.Context, destType, destVersion string, destCfg map[string]string) (map[string]interface{}, bool) {
	callCtx, cancel := context.WithTimeout(ctx, destWritableCheckTimeout)
	defer cancel()
	resp, err := a.executeWithRetry(callCtx, mcp.ExecuteRequest{
		Connector: destType,
		Version:   destVersion,
		Operation: "get_capabilities",
		Config:    destCfg,
		Params:    map[string]interface{}{"config": destCfg},
	})
	if err != nil || resp == nil || !resp.Success {
		return nil, false
	}
	return resp.Result, true
}

// destWriteTargets names the destination resources a sink run writes, bare, the
// way a generated connector matches them (it strips a schema prefix:
// "public.list_task" → "list_task"): the pinned table when the run has one the
// sink honors, else each selected table, which the sink routes to per message.
// A "*" names no resource, so it is never checked.
func destWriteTargets(pinned string, tables []string) []string {
	if t := bareResourceName(pinned); t != "" && t != "*" && !sinkIgnoresTableOverride(pinned) {
		return []string{t}
	}
	var out []string
	seen := map[string]bool{}
	for _, table := range tables {
		if t := bareResourceName(table); t != "" && t != "*" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func bareResourceName(table string) string {
	table = strings.TrimSpace(table)
	if i := strings.LastIndex(table, "."); i >= 0 {
		table = table[i+1:]
	}
	return strings.Trim(strings.TrimSpace(table), "\"`")
}

// sinkIgnoresTableOverride is the sink's isSuspiciousTableOverride (kafka-sink-worker
// main.go): a filler word an NL parser pinned as the table, which the sink drops in
// favour of the source table. TestSinkIgnoresTableOverride_MatchesTheSink keeps the
// two lists equal.
func sinkIgnoresTableOverride(s string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Trim(s, `"`))) {
	case "the", "a", "an", "to", "into", "from", "as", "table", "tables",
		"source", "destination", "dest", "sink", "target", "connection":
		return true
	}
	return false
}

// destResourceNotWritable is the run's error when a target is outside the
// destination's writable_resources, naming the resources it can write;
// "" when every target is writable or the capabilities carry no such list.
func destResourceNotWritable(caps map[string]interface{}, targets []string, destType string) string {
	raw, ok := caps["writable_resources"].([]interface{})
	if !ok {
		return ""
	}
	writable := map[string]bool{}
	var names []string
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" && !writable[s] {
			writable[s] = true
			names = append(names, s)
		}
	}
	var refused []string
	for _, t := range targets {
		if !writable[t] {
			refused = append(refused, fmt.Sprintf("%q", t))
		}
	}
	if len(refused) == 0 {
		return ""
	}
	sort.Strings(names)
	subject := "destination resource " + refused[0] + " is"
	if len(refused) > 1 {
		subject = "destination resources " + strings.Join(refused, ", ") + " are"
	}
	if len(names) == 0 {
		return fmt.Sprintf("%s not writable on %s: it takes writes to no resource", subject, destType)
	}
	return fmt.Sprintf("%s not writable on %s: it takes writes to %s. Name one as the destination (e.g. \"… as %s\")",
		subject, destType, strings.Join(names, ", "), names[0])
}
