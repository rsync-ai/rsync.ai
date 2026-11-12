package notifier

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

// stubNotifierDial replaces the notifier's dial for one test and shrinks the
// wait between dials.
func stubNotifierDial(t *testing.T, dial func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error)) {
	t.Helper()
	for _, k := range []string{
		"KAFKA_BROKERS", "KAFKA_BOOTSTRAP_SERVERS", "KAFKA_CLIENT_ID",
		"KAFKA_SECURITY_PROTOCOL", "KAFKA_SASL_MECHANISM",
		"KAFKA_SASL_USERNAME", "KAFKA_SASL_PASSWORD",
		"KAFKA_SSL_CA_LOCATION", "KAFKA_SSL_CERT_LOCATION", "KAFKA_SSL_KEY_LOCATION",
	} {
		t.Setenv(k, "")
	}
	prevDial, prevBackoff := newNotifierConsumerGroup, notifierDialBackoff
	newNotifierConsumerGroup = dial
	notifierDialBackoff = rsynckafka.ConsumerGroupDialBackoff{Initial: time.Millisecond, Max: 4 * time.Millisecond}
	t.Cleanup(func() { newNotifierConsumerGroup, notifierDialBackoff = prevDial, prevBackoff })
}

// The notifier must outlast a Kafka that is still starting.
//
// sarama.NewConsumerGroup connects to a broker before it returns. Pre-fix the
// notifier dialled once, inside Start: on the 2026-10-25 cold boot Kafka took
// about two minutes to accept connections, that one dial was refused, main.go
// logged it and carried on, and no Slack or email alert was delivered until
// somebody restarted the gateway. Nothing reported the outage, because the
// notifier is what reports outages.
//
// db is nil: the channel settings then come from the environment.
func TestStartWaitsForAKafkaThatIsStillStarting(t *testing.T) {
	group := newBootConsumerGroup()
	var dials atomic.Int32
	stubNotifierDial(t, func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error) {
		if dials.Add(1) <= 3 {
			return nil, errBrokerRefused
		}
		return group, nil
	})

	n, err := Start(context.Background(), nil, []string{"127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Start gave up on a broker that was still starting: %v", err)
	}
	t.Cleanup(n.Stop)

	select {
	case topics := <-group.consuming:
		if want := n.topics.all(); !slices.Equal(topics, want) {
			t.Errorf("consuming %v, want %v", topics, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the notifier never started consuming (%d dials)", dials.Load())
	}
	if got := dials.Load(); got != 4 {
		t.Errorf("dialled %d times, want 4: three refusals, then the connect", got)
	}

	n.Stop()
	select {
	case <-group.closed:
	default:
		t.Error("Stop left the consumer group open")
	}
}

// Stopping while Kafka has still not come up must end the dial loop, not wait
// for a broker that may never answer.
func TestStopEndsADialThatIsStillWaiting(t *testing.T) {
	var dials atomic.Int32
	stubNotifierDial(t, func([]string, string, *sarama.Config) (sarama.ConsumerGroup, error) {
		dials.Add(1)
		return nil, errBrokerRefused
	})

	n, err := Start(context.Background(), nil, []string{"127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Start gave up on a broker that was still starting: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dials.Load() < 2 {
		t.Fatalf("dialled %d times, want the notifier to keep dialling", dials.Load())
	}

	start := time.Now()
	n.Stop()
	if took := time.Since(start); took > time.Second {
		t.Errorf("Stop took %s waiting on a dial loop that should have ended at once", took)
	}
	select {
	case <-n.done:
	default:
		t.Error("the dial loop is still running after Stop")
	}
}
