package kafka

import (
	"errors"
	"fmt"
	"strings"

	"github.com/IBM/sarama"
	kafkaclient "github.com/rsync-ai/shared/kafkaclient"
)

// IsAuthorizationError reports whether err is the broker refusing this Kafka user an
// operation by ACL: TOPIC_AUTHORIZATION_FAILED or CLUSTER_AUTHORIZATION_FAILED,
// anywhere in its chain. sarama's CreateTopic returns a *sarama.TopicError whose
// Unwrap is the broker's KError, so errors.Is sees through ensureTopicLocked's %w and
// through the errors.Join of EnsurePlatformTopics. Only the error code counts, never
// the message text.
func IsAuthorizationError(err error) bool {
	return errors.Is(err, sarama.ErrTopicAuthorizationFailed) ||
		errors.Is(err, sarama.ErrClusterAuthorizationFailed)
}

// TopicCreateRefusedError is a topic create the broker refused by ACL. It is not a
// transient failure: no retry makes the topic exist, and broker auto-create needs the
// same Create grant, so a run that needs the topic must stop and say which grant is
// missing (docs/deployment/kafka-acls.md §5).
type TopicCreateRefusedError struct {
	Topic   string
	Purpose string // what the topic is for, e.g. "CDC data"
	Err     error
}

func (e *TopicCreateRefusedError) Error() string {
	return fmt.Sprintf("Kafka refused to create the %s topic %q: the Kafka user rsync.ai connects as is not allowed to create topics. "+
		"Grant it Create on %s (docs/deployment/kafka-acls.md §5), then run the pipeline again (%v)",
		e.Purpose, e.Topic, CreateGrantScope(e.Topic), e.Err)
}

func (e *TopicCreateRefusedError) Unwrap() error { return e.Err }

// CreateGrantScope names the narrowest Create grant that covers topic: the topic
// prefix when the topic is under it, since that one grant covers every topic a
// pipeline names at runtime, otherwise the topic itself.
func CreateGrantScope(topic string) string {
	if prefix := kafkaclient.TopicPrefix(); prefix != "" && strings.HasPrefix(topic, prefix) {
		return fmt.Sprintf("topic prefix %q", prefix)
	}
	return fmt.Sprintf("topic %q", topic)
}

// TopicCreateRefused returns a *TopicCreateRefusedError when err is an ACL refusal,
// and nil for nil or any other error, which the caller keeps as its best-effort
// warning.
func TopicCreateRefused(topic, purpose string, err error) error {
	if !IsAuthorizationError(err) {
		return nil
	}
	return &TopicCreateRefusedError{Topic: topic, Purpose: purpose, Err: err}
}
