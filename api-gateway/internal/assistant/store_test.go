package assistant

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis is the two commands RedisStore uses, over a map.
type fakeRedis struct {
	vals   map[string]string
	ttls   map[string]time.Duration
	getErr error
}

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	if f.getErr != nil {
		return redis.NewStringResult("", f.getErr)
	}
	v, ok := f.vals[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(v, nil)
}

func (f *fakeRedis) Set(_ context.Context, key string, value interface{}, ttl time.Duration) *redis.StatusCmd {
	if f.vals == nil {
		f.vals, f.ttls = map[string]string{}, map[string]time.Duration{}
	}
	switch v := value.(type) {
	case string:
		f.vals[key] = v
	case []byte:
		f.vals[key] = string(v)
	}
	f.ttls[key] = ttl
	return redis.NewStatusResult("OK", nil)
}

func TestRedisStoreKeepsConversationsAndStopMarks(t *testing.T) {
	ctx := context.Background()
	r := &fakeRedis{}
	s := RedisStore{Client: r}

	if turns, err := s.Load(ctx, "conv"); err != nil || turns != nil {
		t.Fatalf("missing conversation: %v %v", turns, err)
	}
	if err := s.Save(ctx, "conv", []Turn{{Question: "q", Answer: "a"}}); err != nil {
		t.Fatal(err)
	}
	if turns, err := s.Load(ctx, "conv"); err != nil || len(turns) != 1 || turns[0].Answer != "a" || r.ttls["conv"] != conversationTTL {
		t.Fatalf("saved conversation: %+v %v ttl %v", turns, err, r.ttls["conv"])
	}
	r.vals["junk"] = "not json"
	if turns, err := s.Load(ctx, "junk"); err != nil || turns != nil {
		t.Fatalf("an unreadable entry must start over: %v %v", turns, err)
	}

	key := stopKey("u1", "req-1")
	if stopped, err := s.Stopped(ctx, key); err != nil || stopped {
		t.Fatalf("unmarked: stopped=%v err=%v", stopped, err)
	}
	if err := s.MarkStopped(ctx, key, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	if stopped, err := s.Stopped(ctx, key); err != nil || !stopped || r.ttls[key] != 3*time.Minute {
		t.Fatalf("marked: stopped=%v err=%v ttl %v", stopped, err, r.ttls[key])
	}

	r.getErr = errors.New("redis down")
	if stopped, err := s.Stopped(ctx, key); err == nil || stopped {
		t.Fatalf("redis down: stopped=%v err=%v, want false and the error", stopped, err)
	}
	if _, err := s.Load(ctx, "conv"); err == nil {
		t.Fatal("redis down: Load must say so")
	}
}

func TestNoStoreCannotCarryAStopToAnotherReplica(t *testing.T) {
	var s NoStore
	if err := s.MarkStopped(context.Background(), "k", time.Minute); err == nil {
		t.Fatal("NoStore must refuse a stop mark it cannot keep")
	}
	if stopped, err := s.Stopped(context.Background(), "k"); err != nil || stopped {
		t.Fatalf("NoStore.Stopped = %v %v", stopped, err)
	}
}

func TestACancelTheServerCannotRecordSaysSo(t *testing.T) {
	// Not running here and no store to leave a mark in: the caller learns the
	// question goes on to its time limit, rather than a 204 that stopped nothing.
	h := newHarness(t, Limits{})
	h.store.markErr = errors.New("redis down")
	w := h.cancel(t, "u1", "req-9")
	if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "stop_unavailable" {
		t.Fatalf("status %d code %q, want 503 stop_unavailable", w.Code, errorCode(t, w))
	}
}
