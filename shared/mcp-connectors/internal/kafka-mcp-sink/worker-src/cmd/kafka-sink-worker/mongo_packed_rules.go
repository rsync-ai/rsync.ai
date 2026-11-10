package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rsync-ai/shared/transforms"
)

// Consumer rules on a MongoDB CDC row, and the mask guard
//
// A MongoDB change event reaches the chain packed as {_id, document}: the
// whole document is one column (decodeMongoDocument). mask_pii and
// type_convert run inside the document. Every other rule names a top-level
// column that the packed row does not have, so a filter passed every row and a
// select emptied each one without a word; such a chain is refused instead and
// the message goes to the DLQ (transforms.RulesUnsupportedOnPackedDocument).
//
// Every mask_pii step, packed or not, runs under transforms.GuardMaskStep: a
// value still sitting where a target reaches fails the message rather than
// reaching the destination. And a target that has matched nothing in its
// first maskUnmatchedWarnRows rows is logged once, since such a rule masks
// nothing (noteMaskMatches).

// errPackedMongoRuleUnsupported marks a MongoDB CDC chain holding a rule that
// cannot run on a packed row.
var errPackedMongoRuleUnsupported = errors.New("consumer rule cannot run on a MongoDB CDC row")

// refusePackedMongoChain returns an error naming the rule types a packed
// MongoDB row cannot run, or nil when the whole chain can.
func refusePackedMongoChain(canonical []transforms.CanonicalTransform) error {
	bad := transforms.RulesUnsupportedOnPackedDocument(canonical)
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s; a MongoDB change is kept as {_id, document} and only mask_pii and "+
		"type_convert run inside the document, so remove the other rule(s) from this pipeline",
		errPackedMongoRuleUnsupported, strings.Join(bad, ", "))
}

// packedDocuments returns the documents of packed MongoDB rows and the index of
// the row each came from. A row with no document map (a delete with no
// pre-image) is skipped.
func packedDocuments(rows []transforms.Row) ([]transforms.Row, []int) {
	docs := make([]transforms.Row, 0, len(rows))
	at := make([]int, 0, len(rows))
	for i, r := range rows {
		if doc, ok := r["document"].(map[string]interface{}); ok {
			docs = append(docs, transforms.Row(doc))
			at = append(at, i)
		}
	}
	return docs, at
}

// applyInsidePackedDocuments runs apply over the documents of packed MongoDB
// rows and puts each result back in a copy of its row. apply must return one
// document per document: the rules that run here never add or drop rows.
func applyInsidePackedDocuments(rows []transforms.Row, apply func([]transforms.Row) ([]transforms.Row, error)) ([]transforms.Row, error) {
	docs, at := packedDocuments(rows)
	if len(docs) == 0 {
		return rows, nil
	}
	changed, err := apply(docs)
	if err != nil {
		return nil, err
	}
	if len(changed) != len(docs) {
		return nil, fmt.Errorf("a rule on packed mongodb rows returned %d documents for %d", len(changed), len(docs))
	}
	next := make([]transforms.Row, len(rows))
	copy(next, rows)
	for j, i := range at {
		row := make(transforms.Row, len(rows[i]))
		for k, v := range rows[i] {
			row[k] = v
		}
		row["document"] = map[string]interface{}(changed[j])
		next[i] = row
	}
	return next, nil
}

// guardedMask runs one mask_pii config under the residual-plaintext guard and
// returns its per-target match counts.
func guardedMask(ctx context.Context, engine *transforms.SimpleTransformEngine, rows []transforms.Row, t transforms.CanonicalTransform, cfg map[string]any) ([]transforms.Row, transforms.MaskStats, error) {
	step := transforms.CanonicalTransform{ID: t.ID, Order: t.Order, Type: "mask_pii", Enabled: true, Config: cfg}
	var stats transforms.MaskStats
	out, err := transforms.GuardMaskStep(rows, step, func(in []transforms.Row) ([]transforms.Row, error) {
		masked, st, err := engine.ApplyMaskWithStats(ctx, in, cfg)
		stats = st
		return masked, err
	})
	return out, stats, err
}

// maskPackedMongoRows runs one mask_pii rule over packed MongoDB rows,
// {_id, document} (KI-MONGO-CDC-MASK-SILENT-NOOP). The rule's column names are
// the names of the document's fields, so they are matched inside document, at
// any depth and in any case (the engine's deep mode; a rule that sets deep keeps
// its own choice). They are never matched against the packed row's own _id and
// document keys: those are the sink's key and wrapper, and a field the user
// called "document" must not hash the whole row. An explicit path keeps running
// from the packed row's root ("document.profile.email"), as the engine
// documents it. Each half runs under the guard on what it masks: the column
// half on the documents, the path half on the rows.
func maskPackedMongoRows(ctx context.Context, engine *transforms.SimpleTransformEngine, rows []transforms.Row, t transforms.CanonicalTransform) ([]transforms.Row, transforms.MaskStats, error) {
	fieldCfg := make(map[string]any, len(t.Config)+1)
	pathCfg := make(map[string]any, len(t.Config))
	for k, v := range t.Config {
		switch k {
		case "path", "paths":
			pathCfg[k] = v
		case "column", "columns", "deep":
			fieldCfg[k] = v
		default:
			fieldCfg[k] = v
			pathCfg[k] = v
		}
	}
	if fieldCfg["deep"] == nil {
		fieldCfg["deep"] = true
	}

	stats := transforms.MaskStats{Rows: len(rows)}
	out := rows
	if maskTargetsNamed(fieldCfg["columns"]) || maskTargetsNamed(fieldCfg["column"]) {
		var fieldStats transforms.MaskStats
		next, err := applyInsidePackedDocuments(out, func(docs []transforms.Row) ([]transforms.Row, error) {
			masked, st, err := guardedMask(ctx, engine, docs, t, fieldCfg)
			fieldStats = st
			return masked, err
		})
		if err != nil {
			return nil, transforms.MaskStats{}, err
		}
		stats.Targets = append(stats.Targets, fieldStats.Targets...)
		out = next
	}
	if maskTargetsNamed(pathCfg["paths"]) || maskTargetsNamed(pathCfg["path"]) {
		next, pathStats, err := guardedMask(ctx, engine, out, t, pathCfg)
		if err != nil {
			return nil, transforms.MaskStats{}, err
		}
		stats.Targets = append(stats.Targets, pathStats.Targets...)
		out = next
	}
	return out, stats, nil
}

// maskTargetsNamed reports whether a mask_pii column/path value names anything.
func maskTargetsNamed(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case []string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return false
}

// maskUnmatchedWarnRows is how many rows a mask target sees with no match
// before the sink says it masks nothing. A CDC stream has no end of run to
// report at, and a target can be absent from the first few events of a table
// and present later.
const maskUnmatchedWarnRows = 1000

type maskMatchKey struct {
	pipeline, table string
	order           int
	target          string
}

type maskMatchCount struct {
	rows, matched int
	warned        bool
}

// maskMatches counts, for this process, the rows each mask target has seen and
// the values it matched. Bounded by the number of rules the sink runs.
var maskMatches = struct {
	sync.Mutex
	m map[maskMatchKey]*maskMatchCount
}{m: map[maskMatchKey]*maskMatchCount{}}

// noteMaskMatches adds one mask step's counts and reports the targets that have
// now seen maskUnmatchedWarnRows rows with no match, each once per process.
func noteMaskMatches(pipelineID, table string, order int, stats transforms.MaskStats) []maskMatchKey {
	if stats.Rows == 0 {
		return nil
	}
	maskMatches.Lock()
	defer maskMatches.Unlock()
	var due []maskMatchKey
	for _, ts := range stats.Targets {
		k := maskMatchKey{pipeline: pipelineID, table: table, order: order, target: ts.Target}
		c, ok := maskMatches.m[k]
		if !ok {
			c = &maskMatchCount{}
			maskMatches.m[k] = c
		}
		c.rows += stats.Rows
		c.matched += ts.Matched
		if !c.warned && c.matched == 0 && c.rows >= maskUnmatchedWarnRows {
			c.warned = true
			due = append(due, k)
		}
	}
	return due
}

// warnUnmatchedMasks logs each target noteMaskMatches reported. Names and
// counts only, never a row value.
func warnUnmatchedMasks(pipelineID, table string, order int, stats transforms.MaskStats) {
	for _, k := range noteMaskMatches(pipelineID, table, order, stats) {
		logEvent("warning", "mask matched no values; the rule masks nothing",
			"pipeline_id", strings.TrimSpace(k.pipeline),
			"table", k.table,
			"rule_order", k.order,
			"target", k.target,
			"rows_seen", maskUnmatchedWarnRows,
			"hint", `check the field name; a nested field needs "deep": true or a "path"`)
	}
}
