package main

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsync-ai/shared/transforms"
)

// cdc_transform_audit.go — the consumer transforms' audit rows, summed in memory and
// written once per window instead of once per row.
//
// Every CDC row a consumer chain ran over used to cost the control-plane database
// 1 + steps synchronous round trips on the consume goroutine: an executions INSERT ...
// ON CONFLICT DO NOTHING, then one transform_execution_logs upsert per step. Each upsert
// adds the row's counts to the step's one row, so a million-row snapshot through a
// two-step chain was three million round trips and two million UPDATEs of the same two
// rows — every one a dead tuple for autovacuum (prod: 693,382 updates on 94 rows). The
// upsert already adds, so writing a window's sums once leaves the same totals.
//
// The window is transformAuditFlushEvery, the relational batcher's flush interval. It is
// not tied to a batch — object storage flushes per file and the warehouse path still
// writes per event — so the audit keeps its own clock: the consume loop writes it on
// every pass once a window has passed (idle passes included), and the shutdown drain
// writes what is left. A crash loses at most one window of audit counts. The audit was
// best-effort before (a failed upsert only logged), and replayed rows count again.

// transformAuditFlushEvery is how often the summed audit rows are written.
const transformAuditFlushEvery = 5 * time.Second

type transformAuditKey struct {
	pipelineID, executionID, table string
	order                          int
}

// transformAuditStep is one step's sums over a window.
type transformAuditStep struct {
	t       transforms.CanonicalTransform // the latest config seen, for the snapshot
	in, out int
	dur     time.Duration
	err     error // the last failure, which is what the per-row upserts left behind
}

// transformAuditTally is the window being summed. record runs on the consume goroutine
// today; the mutex keeps it correct if a transform ever runs on a flush lane.
type transformAuditTally struct {
	mu        sync.Mutex
	steps     map[transformAuditKey]*transformAuditStep
	keys      []transformAuditKey // first-seen order, so writes are deterministic
	lastWrite time.Time
}

// cdcTransformAudit is the worker's tally. A worker runs one pipeline, so one is enough.
var cdcTransformAudit = &transformAuditTally{}

// record adds one step's run over one message to the window.
func (a *transformAuditTally) record(pipelineID, executionID, table string, t transforms.CanonicalTransform, in, out int, dur time.Duration, err error) {
	k := transformAuditKey{strings.TrimSpace(pipelineID), strings.TrimSpace(executionID), strings.TrimSpace(table), t.Order}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.steps == nil {
		a.steps = make(map[transformAuditKey]*transformAuditStep)
	}
	s := a.steps[k]
	if s == nil {
		s = &transformAuditStep{}
		a.steps[k] = s
		a.keys = append(a.keys, k)
	}
	s.t = t
	s.in += in
	s.out += out
	s.dur += dur
	if err != nil {
		s.err = err
	}
}

// writeDue writes the window when it holds something and transformAuditFlushEvery has
// passed since the last write. The first write after start is immediate. A cancelled
// ctx is a worker shutting down: its window is left for the shutdown drain's write,
// since writing it here would fail and drop the sums.
func (a *transformAuditTally) writeDue(ctx context.Context, pgDB *sql.DB, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	due := len(a.keys) > 0 && now.Sub(a.lastWrite) >= transformAuditFlushEvery
	a.mu.Unlock()
	if due {
		a.write(ctx, pgDB, now)
	}
}

// write sends each step's sums as one upsert, after one executions row per execution the
// window names (transform_execution_logs.execution_id has a hard FK to it). The window
// is taken out first and not put back on failure: the audit is best-effort, and holding
// the sums for a retry would hold the consume loop on a struggling database.
func (a *transformAuditTally) write(ctx context.Context, pgDB *sql.DB, now time.Time) {
	a.mu.Lock()
	steps, keys := a.steps, a.keys
	a.steps, a.keys, a.lastWrite = nil, nil, now
	a.mu.Unlock()
	if pgDB == nil {
		return
	}
	ensured := make(map[[2]string]bool, 1)
	for _, k := range keys {
		if e := [2]string{k.pipelineID, k.executionID}; !ensured[e] {
			ensured[e] = true
			ensureExecutionRowForCDCAudit(ctx, pgDB, k.pipelineID, k.executionID)
		}
		s := steps[k]
		upsertTransformExecutionLogPG(ctx, pgDB, k.pipelineID, k.executionID, k.table, s.t, s.in, s.out, s.dur, s.err)
	}
}

// missingColumnWarned holds the "rule names a column no row carries" warnings already
// logged. The check runs per message, and a rule naming a column the table does not have
// fails it on every one: a line per row. Once per pipeline, table, step and column is
// the same news, as warnUnmatchedMasks does for a mask that matches nothing.
var missingColumnWarned sync.Map

// warnMissingColumnOnce logs w for this step unless it already has been.
func warnMissingColumnOnce(pipelineID, table string, t transforms.CanonicalTransform, w string) {
	key := strings.TrimSpace(pipelineID) + "|" + table + "|" + strconv.Itoa(t.Order) + "|" + w
	if _, seen := missingColumnWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	logf("warning", "transform %s (order %d) on %s: %s", t.Type, t.Order, table, w)
}
