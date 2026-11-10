package executor

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/rsync-ai/shared/transforms"
)

// A mask_pii step in a batch run
//
// The engine's contract is "mask if present", so a mask that cannot reach the
// field it names reports success either way. Three things close that here:
//
//   - A MongoDB source masks nested fields by default. A batch MongoDB row is
//     the document itself, so a rule on `email` also means `profile.email` and
//     `cards[].email`. A rule that sets `deep` itself keeps its own value.
//   - The step runs under transforms.GuardMaskStep: a value still sitting where
//     a target reaches fails the run rather than reaching the destination.
//   - Each target's match count is tallied across the dispatch's tables, and a
//     target that saw rows and matched none is a warning on the run's
//     timeline, since such a rule masked nothing.

// maskStepConfigFor returns the config the step runs with: a MongoDB source's
// mask without its own `deep` gets deep: true; anything else is unchanged.
func maskStepConfigFor(sourceType string, t transforms.CanonicalTransform) transforms.CanonicalTransform {
	if t.Type != "mask_pii" || !isMongoSourceFamily(sourceType) {
		return t
	}
	if v, ok := t.Config["deep"]; ok && v != nil {
		return t
	}
	cfg := make(map[string]any, len(t.Config)+1)
	for k, v := range t.Config {
		cfg[k] = v
	}
	cfg["deep"] = true
	t.Config = cfg
	return t
}

// applyMaskStep runs one mask_pii step under the residual-plaintext guard and
// returns its per-target match counts.
func applyMaskStep(ctx context.Context, engine *transforms.SimpleTransformEngine, rows []transforms.Row, step transforms.CanonicalTransform) ([]transforms.Row, transforms.MaskStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, transforms.MaskStats{}, err
	}
	var stats transforms.MaskStats
	out, err := transforms.GuardMaskStep(rows, step, func(in []transforms.Row) ([]transforms.Row, error) {
		masked, st, err := engine.ApplyMaskWithStats(ctx, in, step.Config)
		stats = st
		return masked, err
	})
	return out, stats, err
}

type maskTallyKey struct {
	order  int
	target string
}

type maskTallyEntry struct {
	rows    int
	matched int
	tables  map[string]struct{}
}

// maskMatchTally adds up, per mask rule target, the rows the dispatch's mask
// steps saw and the values they matched, across every table. Safe for the
// per-table goroutines; a nil tally is a no-op.
type maskMatchTally struct {
	mu      sync.Mutex
	entries map[maskTallyKey]*maskTallyEntry
	keys    []maskTallyKey
}

func newMaskMatchTally() *maskMatchTally {
	return &maskMatchTally{entries: map[maskTallyKey]*maskTallyEntry{}}
}

func (t *maskMatchTally) add(order int, table string, stats transforms.MaskStats) {
	if t == nil || stats.Rows == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ts := range stats.Targets {
		k := maskTallyKey{order: order, target: ts.Target}
		e, ok := t.entries[k]
		if !ok {
			e = &maskTallyEntry{tables: map[string]struct{}{}}
			t.entries[k] = e
			t.keys = append(t.keys, k)
		}
		e.rows += stats.Rows
		e.matched += ts.Matched
		e.tables[table] = struct{}{}
	}
}

// maskUnmatched is one mask target that saw rows and matched none of them.
type maskUnmatched struct {
	Order  int
	Target string
	Rows   int
	Tables []string
}

// unmatched lists the targets that saw rows and matched nothing, in the order
// they were first seen.
func (t *maskMatchTally) unmatched() []maskUnmatched {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []maskUnmatched
	for _, k := range t.keys {
		e := t.entries[k]
		if e.rows == 0 || e.matched > 0 {
			continue
		}
		tables := make([]string, 0, len(e.tables))
		for tb := range e.tables {
			tables = append(tables, tb)
		}
		sort.Strings(tables)
		out = append(out, maskUnmatched{Order: k.order, Target: k.target, Rows: e.rows, Tables: tables})
	}
	return out
}

// maskUnmatchedMessage names the rule, the target and the row count; never a
// row value.
func maskUnmatchedMessage(u maskUnmatched) string {
	where := fmt.Sprintf("%d table(s)", len(u.Tables))
	if len(u.Tables) == 1 {
		where = "table " + u.Tables[0]
	}
	return fmt.Sprintf("mask rule %d matched no value for %q in %d row(s) across %s, so it masked nothing; "+
		"check the field name (a nested field needs \"deep\": true or a \"path\")", u.Order, u.Target, u.Rows, where)
}

// buildExecutorMaskUnmatchedEvent is a warning row on the run's timeline for a
// mask target that matched nothing. A rule that ran on one table names it.
func buildExecutorMaskUnmatchedEvent(pipelineID, executionID, traceID string, u maskUnmatched) map[string]interface{} {
	evt := buildExecutorProgressEvent(pipelineID, executionID, traceID, maskUnmatchedMessage(u), 0)
	evt["severity"] = "warning"
	evt["summary"] = "Mask matched no values"
	delete(evt, "progress")
	if len(u.Tables) == 1 {
		evt["table_name"] = u.Tables[0]
	}
	return evt
}
