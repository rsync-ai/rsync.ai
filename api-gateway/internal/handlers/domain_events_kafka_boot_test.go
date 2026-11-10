package handlers

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rsynckafka "api-gateway/internal/kafka"

	"github.com/IBM/sarama"
	"github.com/rsync-ai/shared/kafkaclient"
)

// errBrokerRefused is what sarama.NewConsumerGroup returned on the 2026-10-25
// cold boot, while the broker was still starting.
var errBrokerRefused = errors.New("kafka: client has run out of available brokers to talk to: dial tcp 172.18.0.5:29092: connect: connection refused")

// bootConsumerGroup is a consumer group that connected. Consume reports the
// topics it was asked for and then blocks until the session is cancelled, the
// way a real group sits in a session.
type bootConsumerGroup struct {
	consuming chan []string
	closeOnce sync.Once
	closed    chan struct{}
}

func newBootConsumerGroup() *bootConsumerGroup {
	return &bootConsumerGroup{consuming: make(chan []string, 1), closed: make(chan struct{})}
}

func (g *bootConsumerGroup) Consume(ctx context.Context, topics []string, _ sarama.ConsumerGroupHandler) error {
	select {
	case g.consuming <- topics:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (g *bootConsumerGroup) Errors() <-chan error { return nil }
func (g *bootConsumerGroup) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return nil
}
func (g *bootConsumerGroup) Pause(map[string][]int32)  {}
func (g *bootConsumerGroup) Resume(map[string][]int32) {}
func (g *bootConsumerGroup) PauseAll()                 {}
func (g *bootConsumerGroup) ResumeAll()                {}

// stubDomainEventDial replaces the manager's dial for one test, shrinks the
// wait between dials, and tears the manager down afterwards so the next test
// starts with none installed.
func stubDomainEventDial(t *testing.T, dial func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error)) {
	t.Helper()
	clearKafkaEnvForTest(t)
	prevDial, prevBackoff := newDomainEventConsumerGroup, domainEventDialBackoff
	newDomainEventConsumerGroup = dial
	domainEventDialBackoff = rsynckafka.ConsumerGroupDialBackoff{Initial: time.Millisecond, Max: 4 * time.Millisecond}
	t.Cleanup(func() {
		ShutdownDomainEventManager()
		eventManager = nil
		newDomainEventConsumerGroup, domainEventDialBackoff = prevDial, prevBackoff
	})
}

// The domain event manager must outlast a Kafka that is still starting.
//
// sarama.NewConsumerGroup connects to a broker before it returns. Pre-fix the
// manager dialled once, inside InitDomainEventManager: on the 2026-10-25 cold
// boot Kafka took about two minutes to accept connections, that one dial was
// refused, main.go logged it and carried on, and no HITL checkpoint reached a
// browser until somebody restarted the gateway.
func TestDomainEventManagerWaitsForAKafkaThatIsStillStarting(t *testing.T) {
	group := newBootConsumerGroup()
	var dials atomic.Int32
	stubDomainEventDial(t, func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error) {
		if dials.Add(1) <= 3 {
			return nil, errBrokerRefused
		}
		return group, nil
	})

	if err := InitDomainEventManager(context.Background(), []string{"127.0.0.1:1"}); err != nil {
		t.Fatalf("InitDomainEventManager gave up on a broker that was still starting: %v", err)
	}
	if eventManager == nil {
		t.Fatal("no manager installed, so a pipeline's event stream has nothing to subscribe to")
	}

	select {
	case topics := <-group.consuming:
		if want := kafkaclient.Topics("pipeline.domain.events"); !slices.Equal(topics, want) {
			t.Errorf("consuming %v, want %v", topics, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the manager never started consuming (%d dials)", dials.Load())
	}
	if got := dials.Load(); got != 4 {
		t.Errorf("dialled %d times, want 4: three refusals, then the connect", got)
	}

	ShutdownDomainEventManager()
	select {
	case <-group.closed:
	default:
		t.Error("shutdown left the consumer group open")
	}
}

// Shutting down while Kafka has still not come up must end the dial loop, not
// wait for a broker that may never answer.
func TestShutdownDomainEventManagerEndsADialThatIsStillWaiting(t *testing.T) {
	var dials atomic.Int32
	stubDomainEventDial(t, func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error) {
		dials.Add(1)
		return nil, errBrokerRefused
	})

	if err := InitDomainEventManager(context.Background(), []string{"127.0.0.1:1"}); err != nil {
		t.Fatalf("InitDomainEventManager gave up on a broker that was still starting: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dials.Load() < 2 {
		t.Fatalf("dialled %d times, want the manager to keep dialling", dials.Load())
	}

	start := time.Now()
	ShutdownDomainEventManager()
	if took := time.Since(start); took > time.Second {
		t.Errorf("shutdown took %s waiting on a dial loop that should have ended at once", took)
	}
	select {
	case <-eventManager.done:
	default:
		t.Error("the dial loop is still running after shutdown")
	}
}
