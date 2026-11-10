package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// Two tables on ONE topic partition — the executor routes every dim_/lookup_ table to
// one `<prefix>.dimensions` topic — so their batches interleave in offset order.
const sharedTopic = "cdc.shop.shared"

func sharedTopicChange(table string, offset int64) (kafka.Message, *SinkMessage) {
	return kafka.Message{Topic: sharedTopic, Partition: 0, Offset: offset},
		&SinkMessage{PipelineID: ackFKPipelineID, Table: "shop." + table, CDCOp: "c", SourceTS: 1_700_000_000_000,
			KeyFields: []string{"id"}, PK: map[string]interface{}{"id": offset},
			After: map[string]interface{}{"id": offset, "name": "a name"}}
}

// stampDest is a fake destination that records the durable offset each write stamps:
// object_metadata.rsync_last_offset on an object import, kafka_offset.offset on a
// database write — the value get_cdc_offsets reads back as the restart point.
type stampDest struct {
	mu     sync.Mutex
	stamps []offsetStamp
}

type offsetStamp struct {
	target string // the object key or table the write named
	offset int64
}

func (d *stampDest) RoundTrip(r *http.Request) (*http.Response, error) {
	var req struct {
		Params struct {
			Arguments struct {
				Key            string                 `json:"key"`
				Table          string                 `json:"table"`
				Data           []json.RawMessage      `json:"data"`
				ObjectMetadata map[string]interface{} `json:"object_metadata"`
				KafkaOffset    map[string]interface{} `json:"kafka_offset"`
			} `json:"arguments"`
		} `json:"params"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)
	args := req.Params.Arguments
	d.mu.Lock()
	if s, ok := args.ObjectMetadata["rsync_last_offset"].(string); ok {
		off, _ := strconv.ParseInt(s, 10, 64)
		d.stamps = append(d.stamps, offsetStamp{target: args.Key, offset: off})
	}
	if f, ok := args.KafkaOffset["offset"].(float64); ok {
		d.stamps = append(d.stamps, offsetStamp{target: args.Table, offset: int64(f)})
	}
	d.mu.Unlock()
	result := fmt.Sprintf(`{"success":true,"rows_upserted":%d}`, len(args.Data))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`)),
		Request:    r,
	}, nil
}

// stampsFor is the offsets the writes naming table stamped, in write order.
func (d *stampDest) stampsFor(table string) []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var found []int64
	for _, s := range d.stamps {
		if strings.Contains(s.target, table) {
			found = append(found, s.offset)
		}
	}
	return found
}

// stampFor is the offset the one write naming table stamped.
func (d *stampDest) stampFor(t *testing.T, table string) int64 {
	t.Helper()
	found := d.stampsFor(table)
	if len(found) != 1 {
		t.Fatalf("expected one stamped write for %s, got %v", table, found)
	}
	return found[0]
}

func dbBatcherFor(t *testing.T, rt http.RoundTripper) (*cdcDBBatcher, *highWaterTracker) {
	t.Helper()
	captureFailClosed(t)
	lanes := newFlushLanes(2)
	t.Cleanup(lanes.close)
	c := newCDCTestCounters()
	hw := newHighWaterTracker()
	return &cdcDBBatcher{
		cfg:          &WorkerConfig{PipelineID: ackFKPipelineID, SinkMode: "cdc"},
		destType:     "mongodb",
		destCfg:      map[string]interface{}{},
		params:       cdcBatchingParams{maxEvents: 2000, maxBytes: 24 << 20, flushInterval: time.Hour, backoff: time.Millisecond},
		gate:         newCommitGate(nil, hw),
		httpClient:   &http.Client{Transport: rt},
		ddl:          &DDLSupport{},
		eventsWriter: &kafka.Writer{},
		metrics:      c.metrics,
		cdcInserts:   c.inserts,
		cdcUpdates:   c.updates,
		cdcDeletes:   c.deletes,
		cdcBytes:     &sync.Map{},
		batches:      map[string]*cdcDBBatch{},
		lanes:        lanes,
	}, hw
}

// An object batch that lands while an EARLIER change of its partition is still held
// in another table's open batch must not mark the partition written past that change.
// Before the fix it committed and stamped its own last offset; the destination seed
// and the Kafka commit both keep the max, so a crash before the held batch landed
// skipped it on restart — lost, with nothing in the DLQ.
func TestCommitGate_ObjectBatchStopsBelowAnotherTablesHeldChange(t *testing.T) {
	dest := &stampDest{}
	b, hw := objectBatcherFor(t, map[string]interface{}{"bucket": "b", "file_format": "jsonl"}, nil, dest)
	ctx := t.Context()
	for i, table := range []string{"alpha", "bravo"} {
		msg, sm := sharedTopicChange(table, int64(i))
		b.add(ctx, msg, sm)
	}
	flushTable := func(table string) {
		for key, batch := range b.batches {
			if batch.table == "shop."+table {
				b.flushBatch(ctx, key, batch, "test")
				return
			}
		}
		t.Fatalf("no open batch for %s", table)
	}

	flushTable("bravo") // offset 1 lands; alpha's offset 0 is still held
	if hw.seen(sharedTopic, 0, 0) {
		t.Fatal("bravo's write marked offset 0 written while alpha still held it: a restart would skip it")
	}
	if got := dest.stampFor(t, "bravo"); got >= 0 {
		t.Fatalf("bravo's object stamped rsync_last_offset=%d; a restart seeded from it skips alpha's held offset 0", got)
	}

	flushTable("alpha") // nothing held now: the partition is written through offset 1
	if got := dest.stampFor(t, "alpha"); got != 1 {
		t.Fatalf("alpha's object stamped rsync_last_offset=%d, want 1 (everything through bravo's offset has landed)", got)
	}
	if !hw.seen(sharedTopic, 0, 1) {
		t.Fatal("with both batches landed, offset 1 must count as written")
	}
}

// The relational batcher has the same hazard through its flush lanes: a later table's
// batch can be submitted, and land, while an earlier table's batch is still open.
func TestCommitGate_DBBatchStopsBelowAnotherTablesHeldChange(t *testing.T) {
	dest := &stampDest{}
	b, hw := dbBatcherFor(t, dest)
	ctx := t.Context()
	for i, table := range []string{"alpha", "bravo"} {
		msg, sm := sharedTopicChange(table, int64(i))
		b.add(ctx, msg, sm)
	}
	flushTable := func(table string) {
		key := fmt.Sprintf("%s|%d|shop.%s", sharedTopic, 0, table)
		batch := b.batches[key]
		if batch == nil {
			t.Fatalf("no open batch for %s", table)
		}
		b.submitFlush(ctx, key, batch, "test")
		b.lanes.waitAll()
	}

	flushTable("bravo")
	if hw.seen(sharedTopic, 0, 0) {
		t.Fatal("bravo's write marked offset 0 written while alpha still held it: a restart would skip it")
	}
	if got := dest.stampFor(t, "bravo"); got >= 0 {
		t.Fatalf("bravo's write stamped kafka_offset=%d; a restart seeded from it skips alpha's held offset 0", got)
	}

	flushTable("alpha")
	if got := dest.stampFor(t, "alpha"); got != 1 {
		t.Fatalf("alpha's write stamped kafka_offset=%d, want 1 (everything through bravo's offset has landed)", got)
	}
	if !hw.seen(sharedTopic, 0, 1) {
		t.Fatal("with both batches landed, offset 1 must count as written")
	}
}

// When a per-row recovery writes a batch row by row, each row's stamp is the floor if
// that row lands, and never past the batch's own next row: a crash between rows must
// restart at the first row not yet written, even when a later table's batch has already
// landed above it.
func TestCommitGate_PerRowStampStopsBelowTheBatchsNextRow(t *testing.T) {
	dest := &stampDest{}
	b, hw := dbBatcherFor(t, dest)
	ctx := t.Context()
	for _, c := range []struct {
		table  string
		offset int64
	}{{"alpha", 0}, {"bravo", 1}, {"alpha", 2}, {"bravo", 3}} {
		msg, sm := sharedTopicChange(c.table, c.offset)
		b.add(ctx, msg, sm)
	}
	key := func(table string) string { return fmt.Sprintf("%s|%d|shop.%s", sharedTopic, 0, table) }

	bravo := b.batches[key("bravo")]
	b.submitFlush(ctx, key("bravo"), bravo, "test")
	b.lanes.waitAll()
	if got := dest.stampFor(t, "bravo"); got >= 0 {
		t.Fatalf("bravo's write stamped kafka_offset=%d while alpha still held offset 0", got)
	}

	alpha := b.batches[key("alpha")]
	delete(b.batches, key("alpha"))
	b.flushBatchPerRow(ctx, key("alpha"), alpha, "test", errors.New("batch write failed"), nil)
	if got, want := dest.stampsFor("alpha"), []int64{1, 3}; !slices.Equal(got, want) {
		t.Fatalf("alpha's per-row stamps = %v, want %v: row 0 may mark only through offset 1, "+
			"because alpha's own row 2 has not landed yet", got, want)
	}
	if !hw.seen(sharedTopic, 0, 3) {
		t.Fatal("with every batch landed, offset 3 must count as written")
	}
}

// recordingCommit is a commit func that records each offset it commits, and fails the
// calls whose index is in fail.
type recordingCommit struct {
	mu      sync.Mutex
	calls   []int64 // every offset asked for, in call order
	commits []int64 // the offsets committed, in the order they completed
	fail    map[int]bool
}

func (r *recordingCommit) commit(_ context.Context, msgs ...kafka.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := len(r.calls)
	r.calls = append(r.calls, msgs[0].Offset)
	if r.fail[i] {
		return errors.New("broker unavailable")
	}
	r.commits = append(r.commits, msgs[0].Offset)
	return nil
}

func (r *recordingCommit) snapshot() (calls, commits []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls), slices.Clone(r.commits)
}

// Batches land in any order; the partition commits only once nothing below is held.
func TestCommitGate_CommitsOnlyBelowTheLowestHeldBatch(t *testing.T) {
	rec := &recordingCommit{}
	hw := newHighWaterTracker()
	g := newCommitGate(rec.commit, hw)
	ctx := t.Context()

	g.hold(sharedTopic, 0, 0) // alpha's batch
	g.hold(sharedTopic, 0, 1) // bravo's batch
	if got := g.handled(ctx, kafka.Message{Topic: sharedTopic, Partition: 0, Offset: 2}); got != -1 {
		t.Fatalf("a change handled above two held batches gave floor %d, want -1", got)
	}
	if got := g.landed(ctx, sharedTopic, 0, 1, 1); got != -1 {
		t.Fatalf("bravo landing under alpha's held batch gave floor %d, want -1", got)
	}
	if _, commits := rec.snapshot(); len(commits) != 0 {
		t.Fatalf("committed %v while alpha's offset 0 was still held", commits)
	}
	if hw.seen(sharedTopic, 0, 0) {
		t.Fatal("offset 0 marked written while alpha still held it")
	}

	// Another partition is not capped by this one's holds.
	if got := g.handled(ctx, kafka.Message{Topic: sharedTopic, Partition: 1, Offset: 7}); got != 7 {
		t.Fatalf("partition 1's floor = %d, want 7: partition 0's holds must not cap it", got)
	}

	if got := g.landed(ctx, sharedTopic, 0, 0, 0); got != 2 {
		t.Fatalf("alpha landing last gave floor %d, want 2 (everything through the handled offset 2)", got)
	}
	if _, commits := rec.snapshot(); !slices.Equal(commits, []int64{7, 2}) {
		t.Fatalf("commits = %v, want [7 2] (partition 1 at 7, then partition 0 at 2)", commits)
	}
	if !hw.seen(sharedTopic, 0, 2) {
		t.Fatal("with nothing held, offset 2 must count as written")
	}
}

// A change handled on its own (a tombstone, a skip, a DLQ'd poison message) above a
// held batch must not commit past that batch.
func TestCommitGate_HandledChangeStopsBelowAHeldBatch(t *testing.T) {
	rec := &recordingCommit{}
	g := newCommitGate(rec.commit, newHighWaterTracker())
	ctx := t.Context()
	g.hold(sharedTopic, 0, 5)
	if got := g.handled(ctx, kafka.Message{Topic: sharedTopic, Partition: 0, Offset: 6}); got != 4 {
		t.Fatalf("a change handled at 6 above a batch held at 5 gave floor %d, want 4", got)
	}
	if _, commits := rec.snapshot(); !slices.Equal(commits, []int64{4}) {
		t.Fatalf("commits = %v, want [4]", commits)
	}
}

// Two batches can hold the same first offset; one landing must not release the other.
func TestCommitGate_DuplicateHoldsAreCounted(t *testing.T) {
	g := newCommitGate(nil, newHighWaterTracker())
	ctx := t.Context()
	g.hold(sharedTopic, 0, 5)
	g.hold(sharedTopic, 0, 5)
	if got := g.landed(ctx, sharedTopic, 0, 5, 6); got != 4 {
		t.Fatalf("one of two batches held at 5 landing gave floor %d, want 4: the other still holds 5", got)
	}
	if got := g.landed(ctx, sharedTopic, 0, 5, 7); got != 7 {
		t.Fatalf("the second batch held at 5 landing gave floor %d, want 7", got)
	}
}

// A lower floor must never be committed after a higher one: kafka-go commits each call
// as it comes, so a late, lower commit moves the group's offset back.
func TestCommitGate_CommitsNeverGoBackwards(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var commits []int64
	first := true
	commit := func(_ context.Context, msgs ...kafka.Message) error {
		mu.Lock()
		block := first
		first = false
		mu.Unlock()
		if block { // the first commit is slow: a broker round trip in flight
			close(entered)
			<-release
		}
		mu.Lock()
		commits = append(commits, msgs[0].Offset)
		mu.Unlock()
		return nil
	}
	g := newCommitGate(commit, newHighWaterTracker())
	ctx := t.Context()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); g.handled(ctx, kafka.Message{Topic: sharedTopic, Offset: 5}) }()
	<-entered
	secondDone := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(secondDone)
		g.handled(ctx, kafka.Message{Topic: sharedTopic, Offset: 9})
	}()
	// Give the second landing every chance to commit 9 ahead of the slow 5.
	select {
	case <-secondDone:
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()

	if !slices.Equal(commits, []int64{5, 9}) {
		t.Fatalf("commits completed as %v, want [5 9]: a commit of 5 after 9 moves the group offset back", commits)
	}
}

// A failed commit is not lost: the next landing commits the floor again, even when that
// landing does not raise it.
func TestCommitGate_FailedCommitIsRetriedByTheNextLanding(t *testing.T) {
	rec := &recordingCommit{fail: map[int]bool{0: true}}
	g := newCommitGate(rec.commit, newHighWaterTracker())
	ctx := t.Context()
	g.handled(ctx, kafka.Message{Topic: sharedTopic, Partition: 0, Offset: 3})
	g.hold(sharedTopic, 0, 4)
	if got := g.handled(ctx, kafka.Message{Topic: sharedTopic, Partition: 0, Offset: 5}); got != 3 {
		t.Fatalf("floor = %d, want 3 (the batch held at 4 caps it)", got)
	}
	calls, commits := rec.snapshot()
	if !slices.Equal(calls, []int64{3, 3}) || !slices.Equal(commits, []int64{3}) {
		t.Fatalf("calls = %v, commits = %v; want the failed commit of 3 retried and committed", calls, commits)
	}
}

// A nil gate keeps the old behaviour: no holds, every stamp and floor is the offset itself.
func TestCommitGate_NilGateStampsTheOffset(t *testing.T) {
	var g *commitGate
	ctx := t.Context()
	g.hold(sharedTopic, 0, 5)
	if g.seen(sharedTopic, 0, 0) {
		t.Fatal("a nil gate has seen nothing")
	}
	if got := g.floorIfLanded(sharedTopic, 0, 5, 7); got != 7 {
		t.Fatalf("nil floorIfLanded = %d, want 7", got)
	}
	if got := g.landed(ctx, sharedTopic, 0, 5, 7); got != 7 {
		t.Fatalf("nil landed = %d, want 7", got)
	}
	if got := g.handled(ctx, kafka.Message{Topic: sharedTopic, Offset: 9}); got != 9 {
		t.Fatalf("nil handled = %d, want 9", got)
	}
}

// The gate only works if nothing goes around it. Any other Kafka commit or high-water
// advance in this package marks a partition past changes still held — the bug class.
func TestCommitGate_IsTheOnlyPlaceThatCommitsOrAdvances(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "cdc_commit_gate.go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "CommitMessages" || sel.Sel.Name == "advance") {
				t.Errorf("%s calls .%s directly; commit and advance only through commitGate (KI-CDC-SINK-COMMIT-PAST-HELD-CHANGES)",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files checked: the guard would pass on nothing")
	}
}
