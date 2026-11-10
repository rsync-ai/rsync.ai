package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/segmentio/kafka-go"
)

// cdc_buffer_budget.go — one memory budget for every CDC change a sink worker holds.
//
// The CDC batchers cap each batch, not the worker. A batch is one (topic, partition,
// table) — plus the partition path for object storage — so a worker assigned many tables
// holds one open batch per table, and the per-batch caps multiply: 40 tables under a
// 32 MB file cap is 1.3 GB of file bytes before anything is due. The container has
// 768 MiB. Nothing between the cap and the OOM killer said no.
//
// The budget is that no. Every buffered change is charged its estimated heap on the way
// in and refunded when its batch has been written. When the total passes the budget the
// batcher writes its largest open batches early, and the consume loop does not fetch
// again until it has: both batchers run add() on the consume goroutine, so a flush there
// IS the fetch pause. kafka-go's group reader has no Pause/Resume, and it heartbeats on
// its own goroutine, so pausing by not calling FetchMessage is safe (see flushLanes.submit).
//
// It counts heap, not file bytes, because they differ by up to 6x. A decoded change
// lives three times over — the raw Kafka record, the parsed SinkMessage, and for object
// storage the bronze event — and a small value costs far more as an interface{} in a map
// than as JSON (measured in cdc_buffer_budget_test.go). Counting file bytes would let a
// 128 MB budget hold 700 MB.

// EnvSinkBufferBudgetMB is the per-worker budget in MiB. 0 turns the budget off — the
// kill switch, and exactly the behaviour before it existed. RSYNC_SINK_ like
// RSYNC_SINK_FLUSH_LANES: the sink worker reads it, nothing else does.
const EnvSinkBufferBudgetMB = "RSYNC_SINK_BUFFER_BUDGET_MB"

// defaultSinkBufferBudgetMB leaves most of the 768 MiB container to what the budget does
// not see: the request body a flush marshals (about the file's size again), the lanes'
// in-flight copies on their way out, kafka-go's fetch buffers, and the Go runtime.
const defaultSinkBufferBudgetMB = 128

// budgetLogEvery rate-limits the "flushed early" line: under sustained pressure every
// add can flush, and the line would otherwise be per change.
const budgetLogEvery = time.Minute

// bufferBudget is the running total of estimated heap held in open and in-flight CDC
// batches. A nil *bufferBudget is valid and means "no budget": charges are dropped and
// over() is always false, so callers never need a nil check — the same convention as a
// nil *flushLanes.
type bufferBudget struct {
	limit int64
	// used is atomic because the relational batcher refunds from its flush lanes.
	used atomic.Int64

	// Early-flush accounting for the rate-limited log line. Touched only on the consume
	// goroutine.
	earlyFlushes int64
	lastLog      time.Time
}

// newBufferBudget returns a budget of limit bytes, or nil (no budget) when limit <= 0.
func newBufferBudget(limit int64) *bufferBudget {
	if limit <= 0 {
		return nil
	}
	return &bufferBudget{limit: limit}
}

func (b *bufferBudget) charge(n int64) {
	if b != nil {
		b.used.Add(n)
	}
}

func (b *bufferBudget) refund(n int64) {
	if b != nil {
		b.used.Add(-n)
	}
}

// over reports whether the buffered total has passed the limit.
func (b *bufferBudget) over() bool {
	return b != nil && b.used.Load() > b.limit
}

func (b *bufferBudget) inUse() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// noteEarlyFlush records one early flush and, at most once per budgetLogEvery, says how
// many there were. One line a minute is enough to show a worker is running at its
// budget — and that its files are smaller than the configured cap because of it.
func (b *bufferBudget) noteEarlyFlush(table string, batchBytes int64) {
	if b == nil {
		return
	}
	b.earlyFlushes++
	now := time.Now()
	if !b.lastLog.IsZero() && now.Sub(b.lastLog) < budgetLogEvery {
		return
	}
	logf("info", "cdc buffer budget: %d MiB held against a %d MiB budget, wrote %d batch(es) early since the last report (latest: table=%s, ~%d KiB); raise %s if files are smaller than the configured cap",
		b.inUse()>>20, b.limit>>20, b.earlyFlushes, table, batchBytes>>10, EnvSinkBufferBudgetMB)
	b.earlyFlushes = 0
	b.lastLog = now
}

// resolveBufferBudgetBytes reads EnvSinkBufferBudgetMB. A malformed or negative value
// warns and uses the default rather than failing the worker, like resolveFlushLaneCount:
// an operator typo must not stop the stream.
func resolveBufferBudgetBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(EnvSinkBufferBudgetMB))
	if raw == "" {
		return defaultSinkBufferBudgetMB << 20
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > 1<<20 {
		logf("warning", "ignoring invalid %s=%q, using %d", EnvSinkBufferBudgetMB, raw, defaultSinkBufferBudgetMB)
		return defaultSinkBufferBudgetMB << 20
	}
	return n << 20
}

// ── Estimating what a buffered change holds ─────────────────────────────────────────
//
// The estimate walks the decoded values the way the runtime lays them out (Go 1.24+
// swiss-table maps, boxed interface values) rather than marshalling anything: it runs
// once per change on the consume goroutine, and a json.Marshal there would cost more
// than the change. It is calibrated against measured heap, not exact — the test holds it
// within [0.7, 1.5] of the truth across narrow, wide, string-heavy and schema-carrying
// changes, which is all a budget needs.

const (
	// Every map this worker holds per change is keyed by string and holds a 16-byte
	// value (interface{} or string), so one slot is 32 bytes.
	heapMapSlotBytes = 32
	// A swiss-table group is 8 slots plus an 8-byte control word.
	heapMapGroupSlots = 8
	heapMapGroupBytes = heapMapGroupSlots*heapMapSlotBytes + 8
	// The map header, plus the table and directory a map past one group adds.
	heapMapHeaderBytes = 48
	heapMapTableBytes  = 48
	// A string or slice boxed into an interface{} allocates its 16/24-byte header.
	heapBoxedStringBytes = 16
	heapBoxedSliceBytes  = 24
	// A float64 boxed into an interface{} allocates 8 bytes.
	heapBoxedWordBytes = 8
)

// perChangeOverheadBytes is what every buffered change holds besides its values: the
// SinkMessage and kafka.Message structs, their slots in the batch's slices (which grow
// by doubling, so a quarter again on average), and the handful of short strings parsing
// allocates per change (table, op, trace id, source block).
var perChangeOverheadBytes = int64(unsafe.Sizeof(SinkMessage{})+unsafe.Sizeof(kafka.Message{})) + 3*8*5/4 + 192

// heapSizeClass rounds an allocation up the way the allocator does closely enough for a
// budget: to 8 bytes below 64, to 16 below 256, then to 1/8th of the size.
func heapSizeClass(n int64) int64 {
	switch {
	case n <= 0:
		return 0
	case n < 64:
		return (n + 7) &^ 7
	case n < 256:
		return (n + 15) &^ 15
	default:
		step := n >> 3
		return (n + step - 1) / step * step
	}
}

// heapMapBytes is a map's own storage for n entries: one group up to 8, then the
// smallest power-of-two table that keeps the 7/8 load factor.
func heapMapBytes(n int) int64 {
	if n <= heapMapGroupSlots {
		return heapMapHeaderBytes + heapMapGroupBytes
	}
	slots := 16
	for slots*7/8 < n {
		slots *= 2
	}
	return heapMapHeaderBytes + heapMapTableBytes + int64(slots/heapMapGroupSlots)*heapMapGroupBytes
}

// heapStringBytes is a string's backing bytes. Map keys and decoded values each own one.
func heapStringBytes(s string) int64 {
	return heapSizeClass(int64(len(s)))
}

// heapWalker estimates the heap a set of decoded values holds, counting a map reachable
// twice once: a change's After map is also its Data[0] and, for jsonl, its bronze
// event's "after".
type heapWalker struct {
	seen map[uintptr]struct{}
}

func (w *heapWalker) once(m interface{}) bool {
	p := reflect.ValueOf(m).Pointer()
	if p == 0 {
		return false
	}
	if w.seen == nil {
		w.seen = make(map[uintptr]struct{}, 4)
	}
	if _, dup := w.seen[p]; dup {
		return false
	}
	w.seen[p] = struct{}{}
	return true
}

// object is a string-keyed map's storage, keys and values.
func (w *heapWalker) object(m map[string]interface{}) int64 {
	if m == nil || !w.once(m) {
		return 0
	}
	n := heapMapBytes(len(m))
	for k, v := range m {
		n += heapStringBytes(k) + w.boxed(v)
	}
	return n
}

// stringMap is a map[string]string (ColumnTypes).
func (w *heapWalker) stringMap(m map[string]string) int64 {
	if m == nil || !w.once(m) {
		return 0
	}
	n := heapMapBytes(len(m))
	for k, v := range m {
		n += heapStringBytes(k) + heapStringBytes(v)
	}
	return n
}

// boxed is what an interface{} value holds beyond its own 16 bytes, which the
// containing map slot or slice element already counts.
func (w *heapWalker) boxed(v interface{}) int64 {
	switch x := v.(type) {
	case nil, bool:
		return 0
	case string:
		return heapBoxedStringBytes + heapStringBytes(x)
	case json.Number:
		return heapBoxedStringBytes + heapStringBytes(string(x))
	case float64, int64, int, uint64, int32, float32:
		return heapBoxedWordBytes
	case map[string]interface{}:
		return w.object(x)
	case []interface{}:
		n := heapBoxedSliceBytes + heapSizeClass(int64(cap(x))*16)
		for _, e := range x {
			n += w.boxed(e)
		}
		return n
	case []map[string]interface{}:
		n := heapBoxedSliceBytes + heapSizeClass(int64(cap(x))*8)
		for _, e := range x {
			n += w.object(e)
		}
		return n
	case []string:
		n := heapBoxedSliceBytes + heapSizeClass(int64(cap(x))*16)
		for _, e := range x {
			n += heapStringBytes(e)
		}
		return n
	case []byte:
		return heapBoxedSliceBytes + heapSizeClass(int64(cap(x)))
	default:
		// A type a decoder does not produce. Count a word rather than reflect over it.
		return heapBoxedWordBytes
	}
}

// bufferedChangeBytes estimates the heap one buffered CDC change holds: its raw Kafka
// record, its parsed SinkMessage, and — object storage only, else nil — its bronze
// event. Maps the three share are counted once.
func bufferedChangeBytes(msg kafka.Message, sm *SinkMessage, evt map[string]interface{}) int64 {
	n := perChangeOverheadBytes +
		heapSizeClass(int64(len(msg.Key))) + heapSizeClass(int64(len(msg.Value)))
	for _, h := range msg.Headers {
		n += 40 + heapStringBytes(h.Key) + heapSizeClass(int64(len(h.Value)))
	}
	var w heapWalker
	if sm != nil {
		n += w.object(sm.After) + w.object(sm.Before) + w.object(sm.PK)
		if len(sm.Data) > 0 {
			n += w.boxed(sm.Data)
		}
		n += w.stringMap(sm.ColumnTypes)
		for _, k := range sm.KeyFields {
			n += 16 + heapStringBytes(k)
		}
	}
	n += w.object(evt)
	return n
}
