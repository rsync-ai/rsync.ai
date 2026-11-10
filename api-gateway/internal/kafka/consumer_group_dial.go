package kafka

import (
	"context"
	"time"

	"github.com/IBM/sarama"
	logrus "github.com/sirupsen/logrus"
)

// ConsumerGroupDialBackoff is how long a background consumer waits between
// dials while Kafka is not accepting connections yet: Initial, doubling each
// attempt, capped at Max.
type ConsumerGroupDialBackoff struct {
	Initial time.Duration
	Max     time.Duration
}

// DefaultConsumerGroupDialBackoff retries quickly while a broker is still
// starting and settles at one dial every 30 s if it never comes up.
var DefaultConsumerGroupDialBackoff = ConsumerGroupDialBackoff{Initial: 2 * time.Second, Max: 30 * time.Second}

func (b ConsumerGroupDialBackoff) next(wait time.Duration) time.Duration {
	if wait *= 2; wait > b.Max {
		return b.Max
	}
	return wait
}

// consumerGroupBootWindow is how long a refused dial reads as Kafka still
// starting (Warn) before it reads as Kafka down (Error). The 2026-10-25 cold
// boot on two vCPUs took about two minutes to accept a connection.
const consumerGroupBootWindow = 5 * time.Minute

// ConnectConsumerGroup calls dial until it returns a consumer group or ctx
// ends, and returns ctx's error only in that second case.
//
// sarama.NewConsumerGroup connects to a broker before it returns, so a group
// built once at startup fails whenever the gateway wins the race with Kafka.
// Both consumer groups here did exactly that and then stayed off for the life
// of the process: HITL checkpoints never reached a browser and no Slack or
// email alert went out until somebody restarted the gateway. The gateway must
// not exit over it either, because every other route still works without
// Kafka, so the caller runs this in its consumer goroutine and it waits as
// long as it takes.
func ConnectConsumerGroup(ctx context.Context, name string, backoff ConsumerGroupDialBackoff, dial func() (sarama.ConsumerGroup, error)) (sarama.ConsumerGroup, error) {
	start := time.Now()
	wait := backoff.Initial
	for attempt := 1; ; attempt++ {
		group, err := dial()
		if err == nil {
			if attempt > 1 {
				logrus.Infof("✅ %s connected to Kafka after %d attempts (%s)", name, attempt, time.Since(start).Round(time.Second))
			}
			return group, nil
		}
		entry := logrus.WithError(err).WithField("attempt", attempt)
		if time.Since(start) < consumerGroupBootWindow {
			entry.Warnf("⏳ %s: Kafka not reachable yet, retrying in %s", name, wait)
		} else {
			entry.Errorf("❌ %s: Kafka still not reachable after %s, retrying in %s", name, time.Since(start).Round(time.Second), wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = backoff.next(wait)
	}
}
