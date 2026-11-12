package main

import (
	"context"
	"sync"

	"github.com/segmentio/kafka-go"
)

// commitGate is the one place a CDC worker commits Kafka offsets and raises its
// high-water mark (KI-CDC-SINK-COMMIT-PAST-HELD-CHANGES).
//
// Both durable marks keep the MAX: kafka-go commits offset+1 for the message it is
// handed, and the destination seed (get_cdc_offsets, _rsync_cdc_offsets) takes the
// highest offset a write stamped. Yet one partition's changes sit in several batches
// at once — every table of a shared topic (the executor routes dim_/lookup_ tables to
// one `<prefix>.dimensions` topic), and every partition_by value, op or date of one
// table on object storage — and those batches land in any order. When a later batch
// marked its own last offset, a crash before an earlier batch landed restarted past
// that batch's changes: gone, with nothing in the DLQ.
//
// So a partition is marked only up to its safe floor: the highest offset below which
// every consumed change has landed or been handled. Each open batch is held at its
// first offset until it lands; the floor stops one below the lowest batch still held.
// A crash replays what was written above the floor, so delivery is at-least-once,
// never lossy.
//
// The floor never goes down: a new batch starts above every offset already consumed,
// so a stamp computed before a write (floorIfLanded) is still safe when it lands.
type commitGate struct {
	mu     sync.Mutex
	commit func(ctx context.Context, msgs ...kafka.Message) error // reader.CommitMessages
	hw     *highWaterTracker
	parts  map[string]*gatePartition // key: hwKey(topic, partition)
}

// noHold is the first offset of a change that was never held in a batch: one handled
// on the consume goroutine (skipped, DLQ'd, written alone).
const noHold int64 = -1

type gatePartition struct {
	held map[int64]int // first offset of each batch not yet landed -> how many
	done int64         // highest offset landed or handled; -1 before any
	want int64         // highest floor to commit; -1 before any

	// commitMu orders the commits of one partition, so a lower floor is never sent
	// after a higher one: kafka-go commits each call as it comes, and a later, lower
	// commit would move the group's offset back. Held across the commit call, which is
	// why it is not mu — hold and seen never wait for a broker round trip.
	commitMu  sync.Mutex
	committed int64 // highest floor committed; -1 before any (guarded by commitMu)
}

func newCommitGate(commit func(ctx context.Context, msgs ...kafka.Message) error, hw *highWaterTracker) *commitGate {
	return &commitGate{commit: commit, hw: hw, parts: map[string]*gatePartition{}}
}

// part returns the partition's state, creating it. Callers hold g.mu.
func (g *commitGate) part(topic string, partition int) *gatePartition {
	k := hwKey(topic, partition)
	p := g.parts[k]
	if p == nil {
		p = &gatePartition{held: map[int64]int{}, done: -1, want: -1, committed: -1}
		g.parts[k] = p
	}
	return p
}

// floor is the partition's safe floor if the batch held at first landed through last.
// Callers hold g.mu.
func (p *gatePartition) floor(first, last int64) int64 {
	f := max(p.done, last)
	for start, n := range p.held {
		if start == first {
			n--
		}
		if n > 0 && start-1 < f {
			f = start - 1
		}
	}
	return f
}

// seen reports whether offset is at or below the partition's durable high-water mark.
func (g *commitGate) seen(topic string, partition int, offset int64) bool {
	if g == nil {
		return false
	}
	return g.hw.seen(topic, partition, offset)
}

// hold registers a batch that starts at first and has not landed yet. Call it when
// the batch is created, before its first change can land anywhere else.
func (g *commitGate) hold(topic string, partition int, first int64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.part(topic, partition).held[first]++
}

// floorIfLanded is the offset a write of the batch held at first, through last, may
// stamp as its durable mark. It changes nothing. Pass noHold for a change no batch
// holds. A nil gate stamps last, as before.
func (g *commitGate) floorIfLanded(topic string, partition int, first, last int64) int64 {
	if g == nil {
		return last
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.part(topic, partition).floor(first, last)
}

// landed records that the batch held at first is written (or parked in the DLQ)
// through last, commits the partition's new floor and raises the high-water mark to
// it. It returns the floor; -1 means nothing below the held batches is safe yet.
func (g *commitGate) landed(ctx context.Context, topic string, partition int, first, last int64) int64 {
	if g == nil {
		return last
	}
	g.mu.Lock()
	p := g.part(topic, partition)
	f := p.floor(first, last)
	if n := p.held[first]; n > 1 {
		p.held[first] = n - 1
	} else {
		delete(p.held, first)
	}
	p.done = max(p.done, last)
	p.want = max(p.want, f)
	g.mu.Unlock()

	if f >= 0 {
		g.hw.advance(topic, partition, f)
	}
	g.commitUpTo(ctx, topic, partition, p)
	return f
}

// handled is landed for one consumed change no batch held: skipped as a duplicate,
// parked in the DLQ, ignored, or written on its own.
func (g *commitGate) handled(ctx context.Context, msg kafka.Message) int64 {
	return g.landed(ctx, msg.Topic, msg.Partition, noHold, msg.Offset)
}

// commitUpTo commits the partition's highest floor so far, unless a commit already
// covers it. A failed commit is retried by the next landing, as before the gate.
func (g *commitGate) commitUpTo(ctx context.Context, topic string, partition int, p *gatePartition) {
	if g.commit == nil {
		return
	}
	p.commitMu.Lock()
	defer p.commitMu.Unlock()
	g.mu.Lock()
	want := p.want
	g.mu.Unlock()
	if want <= p.committed {
		return
	}
	// kafka-go commits Offset+1: the next offset to read.
	if err := g.commit(ctx, kafka.Message{Topic: topic, Partition: partition, Offset: want}); err == nil {
		p.committed = want
	}
}
