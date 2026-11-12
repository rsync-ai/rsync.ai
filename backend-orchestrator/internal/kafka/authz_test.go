package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/IBM/sarama"
)

// createRefusal is the error ensureTopicLocked returns when the broker refuses a
// CreateTopics by ACL: sarama's *TopicError wrapped with %w.
func createRefusal(topic string, kerr sarama.KError) error {
	return fmt.Errorf("failed to create topic '%s': %w", topic, &sarama.TopicError{Err: kerr})
}

// TestIsAuthorizationErrorSeesThroughTheWraps: the refusal arrives as a sarama
// *TopicError, wrapped once by ensureTopicLocked and possibly again by a caller, or
// joined by EnsurePlatformTopics. Each shape must read as a refusal; a timeout, a
// missing broker or an "already exists" must not.
func TestIsAuthorizationErrorSeesThroughTheWraps(t *testing.T) {
	refused := []error{
		createRefusal("rsync.cdc-1.shop.orders", sarama.ErrTopicAuthorizationFailed),
		createRefusal("rsync.cdc-1.shop.orders", sarama.ErrClusterAuthorizationFailed),
		fmt.Errorf("pre-create: %w", createRefusal("t", sarama.ErrTopicAuthorizationFailed)),
		errors.Join(errors.New("other topic: request timed out"), createRefusal("t", sarama.ErrTopicAuthorizationFailed)),
		sarama.ErrTopicAuthorizationFailed,
	}
	for _, err := range refused {
		if !IsAuthorizationError(err) {
			t.Errorf("IsAuthorizationError(%v) = false, want true", err)
		}
	}

	notRefused := []error{
		nil,
		createRefusal("t", sarama.ErrRequestTimedOut),
		createRefusal("t", sarama.ErrTopicAlreadyExists),
		fmt.Errorf("failed to list topics: %w", sarama.ErrOutOfBrokers),
		context.DeadlineExceeded,
		// A message that merely mentions authorization is not a refusal: only the
		// broker's error code is.
		errors.New("TOPIC_AUTHORIZATION_FAILED mentioned in passing"),
	}
	for _, err := range notRefused {
		if IsAuthorizationError(err) {
			t.Errorf("IsAuthorizationError(%v) = true, want false", err)
		}
	}
}

// TestTopicCreateRefusedNamesTheGrant: a refusal becomes an error that names the
// topic, what it is for, and the grant that fixes it, and still unwraps to the cause.
// Any other error, and nil, give nil so the caller keeps its best-effort warning.
func TestTopicCreateRefusedNamesTheGrant(t *testing.T) {
	t.Setenv("KAFKA_TOPIC_PREFIX", "acme.")

	cause := createRefusal("acme.cdc-1.shop.orders", sarama.ErrTopicAuthorizationFailed)
	err := TopicCreateRefused("acme.cdc-1.shop.orders", "CDC data", cause)
	if err == nil {
		t.Fatal("TopicCreateRefused returned nil for an ACL refusal")
	}
	msg := err.Error()
	for _, want := range []string{`"acme.cdc-1.shop.orders"`, "CDC data", "Create", `prefix "acme."`, "kafka-acls.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	if !errors.Is(err, sarama.ErrTopicAuthorizationFailed) {
		t.Error("the refusal no longer unwraps to the broker error")
	}
	var refused *TopicCreateRefusedError
	if !errors.As(err, &refused) || refused.Topic != "acme.cdc-1.shop.orders" {
		t.Errorf("errors.As(*TopicCreateRefusedError) = %+v", refused)
	}

	// A topic outside the prefix gets a grant on the topic itself: a prefix grant
	// would not cover it.
	other := TopicCreateRefused("connect-offsets", "Connect offsets", cause).Error()
	if !strings.Contains(other, `topic "connect-offsets"`) || strings.Contains(other, "prefix") {
		t.Errorf("a topic outside the prefix should name the topic grant, got %q", other)
	}

	for _, e := range []error{nil, createRefusal("t", sarama.ErrRequestTimedOut), errors.New("kafka manager not connected")} {
		if got := TopicCreateRefused("t", "CDC data", e); got != nil {
			t.Errorf("TopicCreateRefused(%v) = %v, want nil", e, got)
		}
	}
}
