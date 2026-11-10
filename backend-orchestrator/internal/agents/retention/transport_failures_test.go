package retention

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// KI-RETENTION-DEAD-1: every call this package makes to Kafka used to turn a failure
// into an invented answer: a config write "simulated" as done, an end offset of
// 10000000, six partitions of made-up consumer offsets, consumer group names guessed
// from the topic, and an S3 archive that was only logged. The safety gate that guards
// a retention cut was computed from those inventions, and the cleanup history
// recorded work that never reached the broker. These tests pin the class: a Kafka
// call that fails is an error, and nothing that depends on it proceeds.

// kafkaDown fails every request: status 0 refuses the connection, any other status
// is the HTTP answer.
type kafkaDown struct{ status int }

func (k kafkaDown) RoundTrip(r *http.Request) (*http.Response, error) {
	if k.status == 0 {
		return nil, errors.New("dial tcp 127.0.0.1:8082: connect: connection refused")
	}
	return &http.Response{StatusCode: k.status, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(`{"error_code":50001,"message":"broker unavailable"}`))}, nil
}

// kafkaAnswers serves a fixed JSON body per "METHOD path"; anything else is a 404.
type kafkaAnswers map[string]string

func (k kafkaAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := k[r.Method+" "+r.URL.Path]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, `{"error_code":40401}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(body))}, nil
}

func retentionWith(rt http.RoundTripper) (*Config, *OffsetTracker, *CleanupManager) {
	cfg := DefaultConfig()
	cfg.Archive.Enabled = true
	tracker := NewOffsetTracker(cfg)
	tracker.httpClient = &http.Client{Transport: rt}
	cleanup := NewCleanupManager(cfg, NewSafetyChecker(cfg, tracker))
	cleanup.httpClient = &http.Client{Transport: rt}
	return cfg, tracker, cleanup
}

func staleBulkLoad(consumers []string) *BulkLoadInfo {
	return &BulkLoadInfo{
		ID: "bl-1", Topic: "rsync.cdc-1.shop.orders", EndOffset: 100, MessageCount: 100,
		ExpectedConsumers: consumers, Status: StatusMonitoring,
		RegisteredAt:  time.Now().Add(-48 * time.Hour),
		AllCaughtUpAt: time.Now().Add(-24 * time.Hour),
	}
}

func TestRetentionKafkaFailuresAreErrorsNotInventedResults(t *testing.T) {
	ctx := context.Background()
	const topic = "rsync.cdc-1.shop.orders"

	for _, down := range []kafkaDown{{0}, {http.StatusInternalServerError}} {
		name := "connection refused"
		if down.status != 0 {
			name = "HTTP 500"
		}
		t.Run(name, func(t *testing.T) {
			_, tracker, cleanup := retentionWith(down)

			if got, err := tracker.GetConsumerGroupOffsets(ctx, "g", topic); err == nil {
				t.Errorf("GetConsumerGroupOffsets = %+v, nil; want an error", got)
			}
			if got, err := tracker.GetTopicEndOffset(ctx, topic); err == nil {
				t.Errorf("GetTopicEndOffset = %d, nil; want an error", got)
			}
			if got, err := tracker.GetConsumerGroupsForTopic(ctx, topic); err == nil {
				t.Errorf("GetConsumerGroupsForTopic = %v, nil; want an error", got)
			}
			if got, err := cleanup.getTopicRetention(ctx, topic); err == nil {
				t.Errorf("getTopicRetention = %d, nil; want an error", got)
			}
			if err := cleanup.setTopicConfig(ctx, topic, "retention.ms", "60000"); err == nil {
				t.Error("setTopicConfig = nil; want an error")
			}

			// The safety gate: a consumer whose progress cannot be read is not caught up,
			// whether the consumers are named or discovered.
			for _, consumers := range [][]string{nil, {"rsync.rsync-pipeline-" + topic}} {
				res, err := cleanup.safetyChecker.CanCleanupSafely(ctx, staleBulkLoad(consumers))
				if err == nil {
					t.Errorf("CanCleanupSafely(consumers=%v) = %+v, nil; want an error", consumers, res)
				}
			}

			// Nothing that changes a topic proceeds, and nothing is recorded as changed.
			for _, step := range []struct {
				name string
				run  func(*BulkLoadInfo, *CleanupResult) error
			}{
				{"reduceRetention", func(bl *BulkLoadInfo, r *CleanupResult) error { return cleanup.reduceRetention(ctx, bl, r) }},
				{"archiveToS3", func(bl *BulkLoadInfo, r *CleanupResult) error { return cleanup.archiveToS3(ctx, bl, r) }},
			} {
				bl, result := staleBulkLoad(nil), &CleanupResult{}
				if err := step.run(bl, result); err == nil {
					t.Errorf("%s = nil; want an error", step.name)
				}
				if result.ArchiveLocation != "" || bl.ArchiveLocation != "" {
					t.Errorf("%s reported an archive at %q that was never written", step.name, result.ArchiveLocation)
				}
				if got := cleanup.GetModifiedTopics(); len(got) != 0 {
					t.Errorf("%s recorded %v as modified; RestoreRetention would later write a guessed retention", step.name, got)
				}
			}

			detector := NewBulkLoadDetector(DefaultConfig(), tracker)
			if bl, err := detector.DetectBulkLoad(ctx, topic, "p-1"); err == nil {
				t.Errorf("DetectBulkLoad = %+v, nil; want an error", bl)
			}
		})
	}

	// A topic nobody consumes has no consumer groups: the answer is none, not names
	// guessed from the topic.
	t.Run("no group consumes the topic", func(t *testing.T) {
		_, tracker, _ := retentionWith(kafkaAnswers{
			"GET /consumer-groups":               `["other-group"]`,
			"GET /consumers/other-group/offsets": `{"offsets":[{"topic":"another.topic","partition":0,"offset":5}]}`,
		})
		got, err := tracker.GetConsumerGroupsForTopic(ctx, topic)
		if err != nil || len(got) != 0 {
			t.Errorf("GetConsumerGroupsForTopic = %v, %v; want none and no error", got, err)
		}
	})
}

// Control for the test above: when Kafka answers, the same calls return what it said.
// Without this, code that failed every call would pass the test above.
func TestRetentionKafkaAnswersAreRead(t *testing.T) {
	ctx := context.Background()
	const topic = "t"
	_, tracker, cleanup := retentionWith(kafkaAnswers{
		"GET /topics/t/configs":              `{"retention.ms":"604800000"}`,
		"PUT /topics/t/configs":              `{}`,
		"GET /topics/t/partitions":           `[{"partition":0,"leader":1},{"partition":1,"leader":1}]`,
		"GET /topics/t/partitions/0/offsets": `{"end_offset":40}`,
		"GET /topics/t/partitions/1/offsets": `{"end_offset":60}`,
		"GET /consumer-groups":               `["g1","g2"]`,
		"GET /consumers/g1/offsets":          `{"offsets":[{"topic":"t","partition":0,"offset":40},{"topic":"t","partition":1,"offset":60}]}`,
		"GET /consumers/g2/offsets":          `{"offsets":[{"topic":"other","partition":0,"offset":5}]}`,
	})

	if got, err := cleanup.getTopicRetention(ctx, topic); err != nil || got != 604800000 {
		t.Errorf("getTopicRetention = %d, %v; want 604800000", got, err)
	}
	if err := cleanup.setTopicConfig(ctx, topic, "retention.ms", "60000"); err != nil {
		t.Errorf("setTopicConfig = %v", err)
	}
	if got, err := tracker.GetTopicEndOffset(ctx, topic); err != nil || got != 100 {
		t.Errorf("GetTopicEndOffset = %d, %v; want 100", got, err)
	}
	if got, err := tracker.GetConsumerGroupsForTopic(ctx, topic); err != nil || len(got) != 1 || got[0] != "g1" {
		t.Errorf("GetConsumerGroupsForTopic = %v, %v; want [g1]", got, err)
	}
	bl := staleBulkLoad(nil)
	bl.Topic = topic
	if err := cleanup.reduceRetention(ctx, bl, &CleanupResult{}); err != nil {
		t.Errorf("reduceRetention = %v", err)
	}
	if got := cleanup.GetModifiedTopics(); len(got) != 1 || got[0] != topic {
		t.Errorf("modified topics = %v, want [%s]", got, topic)
	}
}

// inventedResultMarker finds the words the old fallbacks were written with. A new
// transport call that "simulates" an answer instead of returning its error fails
// TestRetentionCodeInventsNoKafkaResult.
var inventedResultMarker = regexp.MustCompile(`(?i)simulat|10000000`)

func TestRetentionCodeInventsNoKafkaResult(t *testing.T) {
	// Arm the scan: it must catch the old shape.
	if !inventedResultMarker.MatchString(`log.Printf("Simulating config set"); return nil`) ||
		!inventedResultMarker.MatchString(`return 10000000, nil`) {
		t.Fatal("the marker scan no longer matches the shape it guards against")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if inventedResultMarker.MatchString(line) {
				t.Errorf("%s:%d invents a Kafka result instead of returning the error: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d source files, want the whole package", scanned)
	}
}
