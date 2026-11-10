package main

// Regression coverage for KI-CDC-1 round 2: the sink must resolve its subscribed
// topics BEFORE joining the consumer group, because kafka-go fixes a generation's
// partition assignment at join time and its partition watcher only reacts to a
// *change* in partition count — a topic that blinks into existence between the
// assignment and the watcher's first read is invisible forever, leaving the member
// consuming nothing while the producer writes into it. See waitForTopicPartitions.
//
// And for KI-SINK-MISSING-TOPIC-REJOIN-STORM: the wait must never end by joining
// with a topic still missing, and the reader's dials are budgeted, because a
// member subscribed to a missing topic rejoins with no backoff and each rejoin
// dials afresh — on a broker with auto-create off that exhausted the shared sink
// container's ephemeral ports.
//
// No live broker needed; the live-kafka case is env-gated per aud4_dlq_test.go.

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func TestTopicResolveTimeoutClamps(t *testing.T) {
	const env = "RSYNC_SINK_TOPIC_WAIT_SECONDS"
	prev, had := os.LookupEnv(env)
	t.Cleanup(func() {
		if had {
			os.Setenv(env, prev)
		} else {
			os.Unsetenv(env)
		}
	})

	cases := []struct {
		name string
		set  string // "" => unset
		want time.Duration
	}{
		{"default when unset", "", 30 * time.Second},
		{"explicit override", "5", 5 * time.Second},
		{"zero disables the wait", "0", 0},
		{"negative clamps to zero", "-10", 0},
		{"above ceiling clamps to 300s", "9000", 300 * time.Second},
		{"garbage falls back to default", "not-a-number", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set == "" {
				os.Unsetenv(env)
			} else {
				os.Setenv(env, tc.set)
			}
			if got := topicResolveTimeout(); got != tc.want {
				t.Fatalf("topicResolveTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// shortTopicWait shrinks the wait's intervals so a test can run well past the
// timeout in under two seconds.
func shortTopicWait(t *testing.T) {
	t.Helper()
	fast, slow, rewarn := topicWaitFastInterval, topicWaitSlowInterval, topicWaitRewarnInterval
	t.Cleanup(func() {
		topicWaitFastInterval, topicWaitSlowInterval, topicWaitRewarnInterval = fast, slow, rewarn
	})
	topicWaitFastInterval, topicWaitSlowInterval, topicWaitRewarnInterval =
		10*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
}

// closedPort returns an address nothing listens on, so every dial fails fast
// with ECONNREFUSED and a topic can never resolve.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestWaitForTopicPartitionsReturnsImmediatelyWhenThereIsNothingToWaitFor(t *testing.T) {
	// An unroutable broker guarantees every dial fails, so anything other than an
	// immediate return would burn the full timeout.
	for _, tc := range []struct {
		name    string
		topics  []string
		timeout time.Duration
	}{
		{"no topics", nil, 30 * time.Second},
		{"only blank topics", []string{"", "   "}, 30 * time.Second},
		{"wait disabled", []string{"some-topic"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			if !waitForTopicPartitions(context.Background(), "127.0.0.1:1", tc.topics, tc.timeout) {
				t.Fatal("returned false, want true: there is nothing to wait for")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("returned after %v, expected an immediate return", elapsed)
			}
		})
	}
}

func TestWaitForTopicPartitionsNeverJoinsWhileATopicIsMissing(t *testing.T) {
	// The bug: at the timeout the wait used to return and the worker joined the
	// group anyway. With auto-create off that member's partition watcher fails on
	// the missing topic, the generation ends without an error, and kafka-go
	// rejoins at once, forever. So the timeout must not end the wait at all.
	shortTopicWait(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- waitForTopicPartitions(ctx, closedPort(t), []string{"topic-that-cannot-exist"}, 100*time.Millisecond)
	}()

	select {
	case joined := <-done:
		t.Fatalf("returned %v while the topic was still missing, 100ms timeout long past: the worker "+
			"would now join a group subscribed to a topic that does not exist", joined)
	case <-time.After(1500 * time.Millisecond):
	}

	// The only way out without the topic is shutdown, and it must not read as a join.
	cancel()
	select {
	case joined := <-done:
		if joined {
			t.Fatal("returned true on shutdown; main would join the group with the topic still missing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still waiting 2s after the context ended; a stop request would hang the worker")
	}
}

func TestWaitForTopicPartitionsJoinsWhenATopicAppearsAfterTheTimeout(t *testing.T) {
	shortTopicWait(t)
	prev := probeTopicPartitions
	t.Cleanup(func() { probeTopicPartitions = prev })

	const timeout = 100 * time.Millisecond
	start := time.Now()
	var probes atomic.Int32
	var resolvedAt atomic.Int64
	probeTopicPartitions = func(_ string, pending map[string]bool) {
		probes.Add(1)
		// The producer creates the topic only well after the timeout.
		if time.Since(start) > 3*timeout {
			resolvedAt.Store(int64(time.Since(start)))
			delete(pending, "late-topic")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !waitForTopicPartitions(ctx, "unused", []string{"late-topic"}, timeout) {
		t.Fatalf("gave up after %d probes; it should keep probing past the timeout and join once the topic exists",
			probes.Load())
	}
	if got := time.Duration(resolvedAt.Load()); got <= 3*timeout {
		t.Fatalf("resolved at %v, want after %v: the fake never got past the timeout", got, 3*timeout)
	}
}

func TestDialBudgetLetsTheBurstThroughThenPaces(t *testing.T) {
	b := newDialBudget(5, 20) // 5 at once, then one every 50ms
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := b.wait(ctx); err != nil {
			t.Fatalf("burst wait %d: %v", i, err)
		}
	}
	if burst := time.Since(start); burst > 40*time.Millisecond {
		t.Fatalf("the burst of 5 took %v; a healthy reader's startup dials must not wait", burst)
	}

	start = time.Now()
	for i := 0; i < 10; i++ {
		if err := b.wait(ctx); err != nil {
			t.Fatalf("paced wait %d: %v", i, err)
		}
	}
	// 10 tokens at 20/s is 500ms; anything well under that means no pacing.
	if paced := time.Since(start); paced < 400*time.Millisecond {
		t.Fatalf("10 dials past the burst took %v, want >= ~500ms: the budget does not pace", paced)
	}
}

func TestDialBudgetWaitEndsWithTheContext(t *testing.T) {
	b := newDialBudget(1, 0.01) // one token, then one every 100s
	if err := b.wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait returned after %v; a reader being closed must not hang on the budget", elapsed)
	}
}

func TestGroupReaderDialerDrawsEveryDialFromABudget(t *testing.T) {
	// The reader main builds must carry the budget; a kgoauth dialer alone has no
	// DialFunc, and kafka-go then dials with a bare net.Dialer, unbounded.
	withSecurity(t, "127.0.0.1:9092")
	d, err := groupReaderDialer("127.0.0.1:9092")
	if err != nil {
		t.Fatalf("groupReaderDialer: %v", err)
	}
	if d.DialFunc == nil {
		t.Fatal("the group reader's dialer has no DialFunc, so its dials are not budgeted")
	}
	if d.SASLMechanism != nil || d.TLS != nil {
		t.Fatal("a PLAINTEXT profile produced TLS or SASL; the wrap changed the security settings")
	}

	// And the budget is what the DialFunc consults: with one token, the second
	// dial waits on it rather than reaching the listener.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	tight := withDialBudget(&kafka.Dialer{}, newDialBudget(1, 0.01))
	c, err := tight.DialFunc(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c, err := tight.DialFunc(ctx, "tcp", l.Addr().String()); err == nil {
		c.Close()
		t.Fatal("second dial connected; it should have waited on the empty budget until the deadline")
	}
}

func TestWaitForTopicPartitionsResolvesAgainstLiveKafka(t *testing.T) {
	broker := liveKafkaBroker(t)
	// The topic is created first, the way the orchestrator pre-creates pipeline
	// topics: the bundled brokers run with auto-create off, so nothing else would.
	topic := "sink-topic-wait-probe-" + time.Now().UTC().Format("20060102150405")
	client := &kafka.Client{Addr: kafka.TCP(broker), Transport: kafkaTransport()}
	if _, err := client.CreateTopics(context.Background(), &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}},
	}); err != nil {
		t.Fatalf("creating %s: %v", topic, err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteTopics(context.Background(), &kafka.DeleteTopicsRequest{Topics: []string{topic}})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if !waitForTopicPartitions(ctx, broker, []string{topic}, 20*time.Second) {
		t.Fatalf("topic %q did not resolve in %v — the pre-join wait is not working", topic, time.Since(start))
	}
}
