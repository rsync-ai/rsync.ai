package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

// Every temporal-adapter restart on the prod VM (8, all within 31s of a reboot on
// 2026-10-22) was this process dialling Kafka before Kafka listened: one refused
// connection, log.Fatalf, and Docker started it again. The Temporal client next to
// it already waited out the same race; the producer did not.

type stubProducer struct{ sarama.SyncProducer }

var errRefused = errors.New("kafka: client has run out of available brokers to talk to: dial tcp 172.18.0.5:29092: connect: connection refused")

func TestKafkaProducerWaitsForABrokerThatIsStillStarting(t *testing.T) {
	calls := 0
	want := &stubProducer{}
	dial := func([]string, *sarama.Config) (sarama.SyncProducer, error) {
		calls++
		if calls < 3 {
			return nil, errRefused
		}
		return want, nil
	}

	got, err := connectKafkaProducer([]string{"kafka:29092"}, sarama.NewConfig(), time.Second, time.Millisecond, dial)
	if err != nil {
		t.Fatalf("a broker that listens on the third attempt still failed startup after %d attempt(s): %v", calls, err)
	}
	if got != want {
		t.Errorf("returned producer %v, want the one the successful dial made", got)
	}
	if calls != 3 {
		t.Errorf("dialled %d times, want 3 (two refusals, then success)", calls)
	}
}

func TestKafkaProducerGivesUpWhenTheBudgetRunsOut(t *testing.T) {
	calls := 0
	dial := func([]string, *sarama.Config) (sarama.SyncProducer, error) {
		calls++
		return nil, errRefused
	}

	start := time.Now()
	_, err := connectKafkaProducer([]string{"kafka:29092"}, sarama.NewConfig(), 30*time.Millisecond, 5*time.Millisecond, dial)
	if err == nil {
		t.Fatal("a broker that never listens was reported as connected")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the dial error did not survive into the startup error: %v", err)
	}
	if calls < 2 {
		t.Errorf("dialled %d time(s), want a retry before giving up", calls)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("gave up after %s, want soon after the 30ms budget", elapsed)
	}
}

// The budget has to outlast the slowest boot this service has been seen to
// need. On the 2026-10-25 cold boot on two vCPUs Kafka took about two minutes
// to accept connections, the 60 s budget ran out, and this process exited once
// more before Docker brought it back.
func TestKafkaStartupBudgetOutwaitsTheObservedColdBoot(t *testing.T) {
	const observedColdBoot = 2 * time.Minute
	if kafkaStartupBudget < observedColdBoot+time.Minute {
		t.Errorf("kafkaStartupBudget is %s, want at least %s: a minute over the %s cold boot of 2026-10-25",
			kafkaStartupBudget, observedColdBoot+time.Minute, observedColdBoot)
	}
	if kafkaStartupWait <= 0 || kafkaStartupWait > 5*time.Second {
		t.Errorf("kafkaStartupWait is %s, want a few seconds", kafkaStartupWait)
	}
}
