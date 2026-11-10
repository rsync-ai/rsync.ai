package main

// The sink's Kafka writers are synchronous, and a synchronous kafka-go writer holds each
// WriteMessages call for BatchTimeout (default 1s) unless BatchSize messages arrive
// first. The sink writes one message at a time on the consume goroutine — a TABLE_STATS
// event per flush, a DLQ record per parked row, a schema-change report per applied drift
// — so each of those writes stalled the stream for a second. These tests time a real
// write through each writer the worker builds, against a broker that answers at once.

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol"
	metadataAPI "github.com/segmentio/kafka-go/protocol/metadata"
	produceAPI "github.com/segmentio/kafka-go/protocol/produce"
)

// instantBroker is a one-partition broker that acknowledges every produce at once, so
// any time a write takes is the writer's own.
type instantBroker struct{ produced atomic.Int32 }

func (b *instantBroker) RoundTrip(_ context.Context, _ net.Addr, req protocol.Message) (protocol.Message, error) {
	switch r := req.(type) {
	case *metadataAPI.Request:
		res := &metadataAPI.Response{Brokers: []metadataAPI.ResponseBroker{{NodeID: 1, Host: "127.0.0.1", Port: 9092}}}
		for _, name := range r.TopicNames {
			res.Topics = append(res.Topics, metadataAPI.ResponseTopic{
				Name:       name,
				Partitions: []metadataAPI.ResponsePartition{{PartitionIndex: 0, LeaderID: 1}},
			})
		}
		return res, nil
	case *produceAPI.Request:
		b.produced.Add(1)
		return &produceAPI.Response{Topics: []produceAPI.ResponseTopic{{
			Topic:      r.Topics[0].Topic,
			Partitions: []produceAPI.ResponsePartition{{Partition: r.Topics[0].Partitions[0].Partition}},
		}}}, nil
	}
	return nil, fmt.Errorf("instantBroker: unexpected %T", req)
}

// timeOneWrite sends one message through w (its transport swapped for an instantBroker)
// and returns how long WriteMessages took.
func timeOneWrite(t *testing.T, w *kafka.Writer) time.Duration {
	t.Helper()
	broker := &instantBroker{}
	w.Transport = broker
	defer w.Close()

	msg := kafka.Message{Key: []byte("k"), Value: []byte(`{"event_type":"TABLE_STATS"}`)}
	if w.Topic == "" {
		msg.Topic = "rsync.cdc.shop.orders.dlq" // the DLQ writer routes per message
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The first write also fetches metadata; time the second, the steady state.
	if err := w.WriteMessages(ctx, msg); err != nil {
		t.Fatalf("warm-up write: %v", err)
	}
	start := time.Now()
	if err := w.WriteMessages(ctx, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	elapsed := time.Since(start)
	if got := broker.produced.Load(); got != 2 {
		t.Fatalf("broker saw %d produce requests, want 2 — the write did not reach it", got)
	}
	return elapsed
}

func TestSyncWriters_OneMessageDoesNotWaitOutKafkaGosBatchTimeout(t *testing.T) {
	cfg := &WorkerConfig{KafkaBootstrapServers: "kafka:29092"}
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")

	// Control: the same events writer with kafka-go's default BatchTimeout. If this does
	// not take about a second, the fixture is not exercising the wait and the bound
	// below would pass for the wrong reason.
	control := newEventsWriter(cfg)
	control.BatchTimeout = 0
	if got := timeOneWrite(t, control); got < 800*time.Millisecond {
		t.Fatalf("control: a write with the default BatchTimeout took %v, want ~1s — fixture broken", got)
	}

	writers := map[string]*kafka.Writer{
		"events":       newEventsWriter(cfg),
		"dlq":          newDLQWriter(cfg),
		"schema-drift": newSchemaDriftWriter(cfg),
	}
	for name, w := range writers {
		if w == nil {
			t.Fatalf("%s: no writer", name)
		}
		if got := timeOneWrite(t, w); got > 250*time.Millisecond {
			t.Errorf("%s writer: one synchronous write took %v, want well under kafka-go's 1s default (BatchTimeout=%v)", name, got, w.BatchTimeout)
		}
	}
}
