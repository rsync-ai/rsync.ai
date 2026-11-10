package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// A conversation is kept server-side for 30 minutes, keyed by user, workspace
// and conversation id, so a follow-up question has the earlier ones. Only each
// question and its final answer are kept, never tool results: a follow-up
// reads fresh data. History a browser sends is never read.

// Turn is one finished question and its answer.
type Turn struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
	// Samples is the model entry that read sample rows for this answer, ""
	// when none did. The answer may quote a value, so a later question sends
	// it only to the same entry while sample rows are still on.
	Samples string `json:"s,omitempty"`
	// Guides are the topic guides the question loaded; the next question
	// loads them again, so a follow-up keeps its area's rules.
	Guides []string `json:"g,omitempty"`
}

// Store keeps conversations, and the marks Stop leaves for a question running
// on another replica.
type Store interface {
	Load(ctx context.Context, key string) ([]Turn, error)
	Save(ctx context.Context, key string, turns []Turn) error
	MarkStopped(ctx context.Context, key string, ttl time.Duration) error
	Stopped(ctx context.Context, key string) (bool, error)
}

const conversationTTL = 30 * time.Minute

func conversationKey(userID, workspaceID, conversationID string) string {
	return "assistant:conv:" + userID + ":" + workspaceID + ":" + conversationID
}

// redisCmds is the part of *redis.Client the store uses.
type redisCmds interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
}

// RedisStore keeps conversations in Redis.
type RedisStore struct {
	Client redisCmds
}

func (s RedisStore) Load(ctx context.Context, key string) ([]Turn, error) {
	raw, err := s.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var turns []Turn
	if err := json.Unmarshal(raw, &turns); err != nil {
		return nil, nil // an unreadable entry starts the conversation over
	}
	return turns, nil
}

func (s RedisStore) Save(ctx context.Context, key string, turns []Turn) error {
	raw, err := json.Marshal(turns)
	if err != nil {
		return err
	}
	return s.Client.Set(ctx, key, raw, conversationTTL).Err()
}

func (s RedisStore) MarkStopped(ctx context.Context, key string, ttl time.Duration) error {
	return s.Client.Set(ctx, key, "1", ttl).Err()
}

func (s RedisStore) Stopped(ctx context.Context, key string) (bool, error) {
	err := s.Client.Get(ctx, key).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return err == nil, err
}

// NoStore keeps nothing: with Redis down every question starts fresh, and
// Stop reaches only a question running on the replica it lands on.
type NoStore struct{}

var errNoStore = errors.New("assistant: no store")

func (NoStore) Load(context.Context, string) ([]Turn, error)             { return nil, nil }
func (NoStore) Save(context.Context, string, []Turn) error               { return nil }
func (NoStore) MarkStopped(context.Context, string, time.Duration) error { return errNoStore }
func (NoStore) Stopped(context.Context, string) (bool, error)            { return false, nil }
