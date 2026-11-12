package handlers

// A model's freshness deadline says how long after its last build it counts as Overdue.
// When its schedule leaves longer than that between two builds, the model goes Overdue
// between every pair of builds while working exactly as configured: Loan portfolio (a 6h
// deadline, rebuilt once a day after the 02:00 UTC Nightly) showed Overdue 18 hours of
// every day. modelRebuildInterval answers what the freshness dialog needs to warn about
// that before the deadline is saved — the longest the schedule can leave between two
// rebuilds.
//
// It lists every scheduled tick that can start a rebuild: the model's own cron or
// interval and, through after_upstream, the ticks of everything above it — pipeline
// schedules, upstream models, and the workflows that run those pipelines or refresh those
// models. The longest gap between consecutive ticks is a FLOOR on the real longest gap
// between rebuilds: a rebuild can only start at one of those ticks, and an 'all' fan-in
// or a condition step that skips some only makes the real gaps longer. So a warning built
// on it is never false. Extra ticks only shorten the answer; a MISSING one would lengthen
// it, so anything whose ticks cannot be listed — a CDC pipeline lands rows when they
// arrive, a ring re-fires itself — makes the answer unknown rather than a guess.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/rsync-ai/shared/pgdriver"
	workflow "github.com/rsync-ai/shared/workflowdef"
	log "github.com/sirupsen/logrus"
)

const (
	// rebuildHorizon is how far ahead ticks are listed: past two years, so a yearly cron
	// shows two ticks and its whole gap, whatever the date.
	rebuildHorizon = 800 * 24 * time.Hour
	// rebuildTicksPerClock stops a dense clock (every minute) from listing a million ticks.
	// A clock that hits it ends the window at its last tick (see longestGap).
	rebuildTicksPerClock = 2000
	// rebuildMaxNodes and rebuildMaxClocks bound one page view's work.
	rebuildMaxNodes  = 200
	rebuildMaxClocks = 32
)

// Why an interval is unknown. Internal: the API answers null for every one of them.
const (
	rebuildUnknownUnpredictable = "unpredictable" // a CDC pipeline in the chain
	rebuildUnknownRing          = "ring"
	rebuildUnknownMissing       = "missing" // a node the loader did not find in the workspace
	rebuildUnknownBadClock      = "bad_clock"
	rebuildUnknownTooMany       = "too_many"
	rebuildUnknownNoSchedule    = "no_schedule" // nothing scheduled can rebuild it
	rebuildUnknownTooRare       = "too_rare"    // fewer than two ticks inside the horizon
)

// rebuildClock is one schedule that ticks: a model or pipeline cron/interval
// (schedule_type + ScheduleSpec, read by nextScheduleRun, which mirrors the Temporal
// schedules both create), or a workflow's schedule trigger (workflow.NextRun).
type rebuildClock struct {
	ScheduleType string // "cron" | "interval" | "workflow"
	Spec         ScheduleSpec
	Trigger      workflow.Trigger
}

func (c rebuildClock) key() string {
	if c.ScheduleType == "workflow" {
		return fmt.Sprintf("workflow|%s|%s", c.Trigger.Cron, c.Trigger.Timezone)
	}
	return fmt.Sprintf("%s|%s|%d|%s", c.ScheduleType, c.Spec.Cron, c.Spec.EverySeconds, c.Spec.Timezone)
}

// next is the first tick strictly after from.
func (c rebuildClock) next(from time.Time) (time.Time, bool) {
	if c.ScheduleType == "workflow" {
		t, err := workflow.NextRun(c.Trigger, from)
		if err != nil || t.IsZero() {
			return time.Time{}, false
		}
		return t, true
	}
	t := nextScheduleRun(c.ScheduleType, c.Spec, from)
	if t == nil {
		return time.Time{}, false
	}
	return *t, true
}

// rebuildNode is a model, pipeline or workflow ("model:<id>", "pipeline:<id>",
// "workflow:<id>"). It starts on each tick of its own Clocks and on each start of the
// nodes in From.
type rebuildNode struct {
	Unpredictable bool
	Clocks        []rebuildClock
	From          []string
}

type rebuildGraph map[string]*rebuildNode

func (g rebuildGraph) node(key string) *rebuildNode {
	n, ok := g[key]
	if !ok {
		n = &rebuildNode{}
		g[key] = n
	}
	return n
}

type rebuildInterval struct {
	Known   bool
	Seconds int64
	Reason  string // set when !Known
}

func unknownInterval(reason string) rebuildInterval {
	return rebuildInterval{Reason: reason}
}

// clocksAbove gathers the distinct clocks that can start node key, or a reason (one of
// the rebuildUnknown* values) when the answer cannot be trusted.
func (g rebuildGraph) clocksAbove(key string) ([]rebuildClock, string) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	seen := map[string]bool{}
	var clocks []rebuildClock
	var walk func(k string) string
	walk = func(k string) string {
		switch state[k] {
		case visiting:
			return rebuildUnknownRing
		case done:
			return ""
		}
		n, ok := g[k]
		if !ok {
			return rebuildUnknownMissing
		}
		if n.Unpredictable {
			return rebuildUnknownUnpredictable
		}
		state[k] = visiting
		if len(state) > rebuildMaxNodes {
			return rebuildUnknownTooMany
		}
		for _, c := range n.Clocks {
			if !seen[c.key()] {
				seen[c.key()] = true
				clocks = append(clocks, c)
			}
		}
		for _, from := range n.From {
			if reason := walk(from); reason != "" {
				return reason
			}
		}
		state[k] = done
		return ""
	}
	if reason := walk(key); reason != "" {
		return nil, reason
	}
	return clocks, ""
}

// longestScheduledGap is the longest gap between consecutive ticks of every clock that
// can rebuild model modelID, over [now, now+rebuildHorizon].
func longestScheduledGap(g rebuildGraph, modelID string, now time.Time) rebuildInterval {
	clocks, reason := g.clocksAbove("model:" + modelID)
	if reason != "" {
		return unknownInterval(reason)
	}
	if len(clocks) == 0 {
		return unknownInterval(rebuildUnknownNoSchedule)
	}
	if len(clocks) > rebuildMaxClocks {
		return unknownInterval(rebuildUnknownTooMany)
	}
	return longestGap(clocks, now)
}

func longestGap(clocks []rebuildClock, now time.Time) rebuildInterval {
	// A clock that stopped at the tick cap was listed only up to its last tick; past
	// that, the gaps would be missing its ticks and come out too long. So the window
	// ends where the shortest-listed clock ends.
	end := now.Add(rebuildHorizon)
	var ticks []time.Time
	for _, c := range clocks {
		t, ok := c.next(now)
		if !ok {
			// It was accepted when saved but cannot be listed here. Leaving it out would
			// lengthen the gaps, so the answer is unknown.
			return unknownInterval(rebuildUnknownBadClock)
		}
		n := 0
		for !t.After(end) {
			ticks = append(ticks, t)
			n++
			if n == rebuildTicksPerClock {
				end = t
				break
			}
			nt, ok := c.next(t)
			if !ok || !nt.After(t) {
				break
			}
			t = nt
		}
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i].Before(ticks[j]) })
	var longest time.Duration
	count := 0
	var prev time.Time
	for _, t := range ticks {
		if t.After(end) {
			break
		}
		if count > 0 && t.Sub(prev) > longest {
			longest = t.Sub(prev)
		}
		prev = t
		count++
	}
	if count < 2 {
		return unknownInterval(rebuildUnknownTooRare)
	}
	return rebuildInterval{Known: true, Seconds: int64(longest / time.Second)}
}

// modelRebuildInterval loads what sits above a model and returns the longest its
// schedule can leave between two rebuilds.
func modelRebuildInterval(ctx context.Context, database *sql.DB, workspaceID, modelID string, now time.Time) (rebuildInterval, error) {
	g, err := loadRebuildGraph(ctx, database, workspaceID, modelID)
	if err != nil {
		return rebuildInterval{}, err
	}
	return longestScheduledGap(g, modelID, now), nil
}

// loadRebuildGraph reads the graph in three queries: the models above modelID, the
// workspace's active workflows, and the pipelines either of them names. Every read is
// scoped to the workspace; a node outside it stays out of the graph, and a walk that
// reaches it answers unknown.
func loadRebuildGraph(ctx context.Context, database *sql.DB, workspaceID, modelID string) (rebuildGraph, error) {
	g := rebuildGraph{}
	pipelineIDs := map[string]bool{}

	// 1. The model and every model above it through ACTIVE after_upstream schedules —
	// a paused schedule starts nothing, so nothing above it can reach the model that way.
	rows, err := database.QueryContext(ctx, `
		WITH RECURSIVE closure(model_id) AS (
			SELECT $2::uuid
			UNION
			SELECT u.upstream_saved_query_id
			FROM closure c
			JOIN saved_query_schedules s ON s.saved_query_id = c.model_id
			     AND s.status = 'active' AND s.schedule_type = 'after_upstream'
			JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
			WHERE u.upstream_saved_query_id IS NOT NULL
		)
		SELECT c.model_id::text,
		       s.schedule_type,
		       s.schedule_spec,
		       u.schedule_id IS NOT NULL,
		       u.upstream_pipeline_id::text,
		       u.upstream_saved_query_id::text
		FROM closure c
		JOIN saved_queries q ON q.id = c.model_id AND q.workspace_id = $1
		LEFT JOIN saved_query_schedules s ON s.saved_query_id = c.model_id AND s.status = 'active'
		LEFT JOIN saved_query_schedule_upstreams u ON s.schedule_type = 'after_upstream'
		     AND u.schedule_id = s.schedule_id
	`, workspaceID, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id                       string
			scheduleType             sql.NullString
			spec                     []byte
			hasEdge                  bool
			upPipeline, upSavedQuery sql.NullString
		)
		if err := rows.Scan(&id, &scheduleType, &spec, &hasEdge, &upPipeline, &upSavedQuery); err != nil {
			return nil, err
		}
		n := g.node("model:" + id)
		switch scheduleType.String {
		case "cron", "interval":
			var s ScheduleSpec
			if err := json.Unmarshal(spec, &s); err != nil {
				n.Unpredictable = true
				continue
			}
			n.Clocks = append(n.Clocks, rebuildClock{ScheduleType: scheduleType.String, Spec: s})
		case scheduleAfterUpstream:
			switch {
			case !hasEdge:
				// After_upstream with no upstream: nothing starts it.
			case upPipeline.Valid:
				n.From = append(n.From, "pipeline:"+upPipeline.String)
				pipelineIDs[upPipeline.String] = true
			case upSavedQuery.Valid:
				n.From = append(n.From, "model:"+upSavedQuery.String)
			default:
				n.Unpredictable = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// 2. Active workflows: a schedule trigger is a clock, after_pipeline starts on its
	// pipeline, and each refresh_model / run_pipeline step starts what it names.
	runs := map[string][]string{} // pipeline id → workflows that run it
	wfRows, err := database.QueryContext(ctx, `
		SELECT id::text, trigger_kind, trigger_pipeline_id::text, definition
		FROM workflows
		WHERE workspace_id = $1 AND status = 'active'
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer wfRows.Close()
	for wfRows.Next() {
		var (
			id, kind  string
			triggerOn sql.NullString
			raw       []byte
		)
		if err := wfRows.Scan(&id, &kind, &triggerOn, &raw); err != nil {
			return nil, err
		}
		var def workflow.Definition
		if err := json.Unmarshal(raw, &def); err != nil {
			// The runner parses the same JSON, so a definition that fails here starts
			// nothing there either.
			log.WithError(err).WithField("workflow_id", id).Warn("rebuild interval: unreadable workflow definition skipped")
			continue
		}
		key := "workflow:" + id
		var refreshes []string
		var pipelines []string
		for _, s := range def.Steps {
			switch {
			case s.Type == workflow.StepRefreshModel && s.ModelID != "":
				if _, ok := g["model:"+s.ModelID]; ok {
					refreshes = append(refreshes, s.ModelID)
				}
			case s.Type == workflow.StepRunPipeline && s.PipelineID != "":
				pipelines = append(pipelines, s.PipelineID)
			}
		}
		for _, m := range refreshes {
			g["model:"+m].From = append(g["model:"+m].From, key)
		}
		for _, p := range pipelines {
			runs[p] = append(runs[p], key)
			pipelineIDs[p] = true
		}
		n := g.node(key)
		switch kind {
		case workflow.TriggerSchedule:
			n.Clocks = append(n.Clocks, rebuildClock{ScheduleType: "workflow", Trigger: def.Trigger})
		case workflow.TriggerAfterPipeline:
			if triggerOn.Valid {
				n.From = append(n.From, "pipeline:"+triggerOn.String)
				pipelineIDs[triggerOn.String] = true
			}
		}
	}
	if err := wfRows.Err(); err != nil {
		return nil, err
	}
	wfRows.Close()

	if len(pipelineIDs) == 0 {
		return g, nil
	}

	// 3. The pipelines named above: CDC is unpredictable, an active schedule is a clock,
	// and the workflows that run it start it too.
	ids := make([]string, 0, len(pipelineIDs))
	for id := range pipelineIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pRows, err := database.QueryContext(ctx, `
		SELECT p.id::text, `+pipelineListIsCDCSQL+`, ps.schedule_type, ps.schedule_spec
		FROM pipelines p
		LEFT JOIN connections sc ON p.source_connection_id = sc.id
		LEFT JOIN pipeline_schedules ps ON ps.pipeline_id = p.id AND ps.status = 'active'
		WHERE p.workspace_id = $1 AND p.id = ANY($2::uuid[])
	`, workspaceID, pgdriver.StringArray(ids))
	if err != nil {
		return nil, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var (
			id           string
			isCDC        bool
			scheduleType sql.NullString
			spec         []byte
		)
		if err := pRows.Scan(&id, &isCDC, &scheduleType, &spec); err != nil {
			return nil, err
		}
		n := g.node("pipeline:" + id)
		n.From = append(n.From, runs[id]...)
		if isCDC {
			n.Unpredictable = true
			continue
		}
		if scheduleType.String == "cron" || scheduleType.String == "interval" {
			var s ScheduleSpec
			if err := json.Unmarshal(spec, &s); err != nil {
				n.Unpredictable = true
				continue
			}
			n.Clocks = append(n.Clocks, rebuildClock{ScheduleType: scheduleType.String, Spec: s})
		}
	}
	return g, pRows.Err()
}
