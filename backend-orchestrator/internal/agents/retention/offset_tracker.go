package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// OffsetTracker tracks consumer group offsets for topics
type OffsetTracker struct {
	config     *Config
	httpClient *http.Client

	// Cache of offsets
	offsets map[string]*ConsumerGroupOffsets // key: "group:topic"
	mu      sync.RWMutex

	// Topic end offsets cache
	endOffsets map[string]int64 // key: topic
	endMu      sync.RWMutex
}

// NewOffsetTracker creates a new offset tracker
func NewOffsetTracker(config *Config) *OffsetTracker {
	// Create HTTP client for Kafka REST proxy (if available)
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.DialTimeout(network, addr, 10*time.Second)
			},
		},
		Timeout: 30 * time.Second,
	}

	return &OffsetTracker{
		config:     config,
		httpClient: httpClient,
		offsets:    make(map[string]*ConsumerGroupOffsets),
		endOffsets: make(map[string]int64),
	}
}

// GetConsumerProgress gets progress for a consumer group on a topic
func (t *OffsetTracker) GetConsumerProgress(
	ctx context.Context,
	consumerGroup, topic string,
	targetOffset int64,
) (*ConsumerProgress, error) {
	offsets, err := t.GetConsumerGroupOffsets(ctx, consumerGroup, topic)
	if err != nil {
		return nil, err
	}

	currentOffset := offsets.TotalOffset
	if currentOffset < 0 {
		currentOffset = 0
	}
	if targetOffset < 0 {
		targetOffset = 0
	}
	lag := targetOffset - currentOffset
	if lag < 0 {
		lag = 0
	}

	var progress float64
	if targetOffset > 0 {
		progress = float64(currentOffset) / float64(targetOffset) * 100
		if progress > 100 {
			progress = 100
		}
	}

	return &ConsumerProgress{
		ConsumerGroup: consumerGroup,
		Topic:         topic,
		CurrentOffset: currentOffset,
		TargetOffset:  targetOffset,
		Lag:           lag,
		Progress:      progress,
		CaughtUp:      currentOffset >= targetOffset,
		LastUpdated:   time.Now(),
	}, nil
}

// GetConsumerGroupOffsets gets offsets for a consumer group on a topic
func (t *OffsetTracker) GetConsumerGroupOffsets(
	ctx context.Context,
	consumerGroup, topic string,
) (*ConsumerGroupOffsets, error) {
	key := fmt.Sprintf("%s:%s", consumerGroup, topic)

	// Check cache first
	t.mu.RLock()
	cached, exists := t.offsets[key]
	t.mu.RUnlock()

	if exists && time.Since(cached.LastCommit) < time.Duration(t.config.Monitoring.CheckIntervalSecs)*time.Second {
		return cached, nil
	}

	// Fetch fresh offsets
	offsets, err := t.fetchConsumerGroupOffsets(ctx, consumerGroup, topic)
	if err != nil {
		// If we have cached data, return it with a warning
		if exists {
			log.Printf("[OffsetTracker] Using cached offsets for %s (fetch error: %v)", key, err)
			return cached, nil
		}
		return nil, err
	}

	// Update cache
	t.mu.Lock()
	t.offsets[key] = offsets
	t.mu.Unlock()

	return offsets, nil
}

// fetchConsumerGroupOffsets fetches offsets from Kafka. A failed read is an error:
// offsets made up here would decide whether a consumer is caught up and its data can go.
func (t *OffsetTracker) fetchConsumerGroupOffsets(
	ctx context.Context,
	consumerGroup, topic string,
) (*ConsumerGroupOffsets, error) {
	offsets, err := t.fetchFromRestProxy(ctx, consumerGroup, topic)
	if err != nil {
		return nil, fmt.Errorf("read the offsets of consumer group %s: %w", consumerGroup, err)
	}
	return offsets, nil
}

// fetchFromRestProxy fetches offsets from Kafka REST proxy
func (t *OffsetTracker) fetchFromRestProxy(
	ctx context.Context,
	consumerGroup, topic string,
) (*ConsumerGroupOffsets, error) {
	// Kafka REST proxy endpoint
	url := fmt.Sprintf("http://localhost:8082/consumers/%s/offsets", consumerGroup)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("REST proxy returned %d", resp.StatusCode)
	}

	var result struct {
		Offsets []struct {
			Topic     string `json:"topic"`
			Partition int32  `json:"partition"`
			Offset    int64  `json:"offset"`
		} `json:"offsets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	offsets := &ConsumerGroupOffsets{
		ConsumerGroup: consumerGroup,
		Topic:         topic,
		Offsets:       make([]PartitionOffset, 0),
		LastCommit:    time.Now(),
	}

	for _, o := range result.Offsets {
		if o.Topic == topic {
			if o.Offset < 0 {
				// Kafka REST proxy may report -1 for unknown offsets; never propagate negatives.
				continue
			}
			offsets.Offsets = append(offsets.Offsets, PartitionOffset{
				Partition: o.Partition,
				Offset:    o.Offset,
			})
			offsets.TotalOffset += o.Offset
		}
	}

	return offsets, nil
}

// GetTopicEndOffset gets the end offset for a topic
func (t *OffsetTracker) GetTopicEndOffset(ctx context.Context, topic string) (int64, error) {
	// Check cache
	t.endMu.RLock()
	cached, exists := t.endOffsets[topic]
	t.endMu.RUnlock()

	if exists {
		return cached, nil
	}

	// Fetch end offset
	endOffset, err := t.fetchTopicEndOffset(ctx, topic)
	if err != nil {
		return 0, err
	}

	// Update cache
	t.endMu.Lock()
	t.endOffsets[topic] = endOffset
	t.endMu.Unlock()

	return endOffset, nil
}

// fetchTopicEndOffset sums the end offsets of the topic's partitions. Every read must
// succeed: a partition left out of the sum understates the topic, and a consumer
// compared with it reads as caught up before it is.
func (t *OffsetTracker) fetchTopicEndOffset(ctx context.Context, topic string) (int64, error) {
	var partitions []struct {
		Partition int32 `json:"partition"`
		Leader    int   `json:"leader"`
	}
	if err := t.getJSON(ctx, fmt.Sprintf("http://localhost:8082/topics/%s/partitions", topic), &partitions); err != nil {
		return 0, fmt.Errorf("list the partitions of %s: %w", topic, err)
	}
	if len(partitions) == 0 {
		return 0, fmt.Errorf("list the partitions of %s: the REST proxy returned none", topic)
	}

	var totalEndOffset int64
	for _, p := range partitions {
		var offsetResult struct {
			EndOffset int64 `json:"end_offset"`
		}
		url := fmt.Sprintf("http://localhost:8082/topics/%s/partitions/%d/offsets", topic, p.Partition)
		if err := t.getJSON(ctx, url, &offsetResult); err != nil {
			return 0, fmt.Errorf("read the end offset of %s partition %d: %w", topic, p.Partition, err)
		}
		totalEndOffset += offsetResult.EndOffset
	}
	return totalEndOffset, nil
}

// GetConsumerGroupsForTopic returns the consumer groups with committed offsets on
// topic, and none when no group consumes it. A group whose offsets cannot be read is
// an error rather than a group left out: the caller asks whether every consumer is
// caught up, and a consumer it never hears about cannot hold a cleanup back.
func (t *OffsetTracker) GetConsumerGroupsForTopic(ctx context.Context, topic string) ([]string, error) {
	var groups []string
	if err := t.getJSON(ctx, "http://localhost:8082/consumer-groups", &groups); err != nil {
		return nil, fmt.Errorf("list consumer groups: %w", err)
	}

	var result []string
	for _, group := range groups {
		offsets, err := t.GetConsumerGroupOffsets(ctx, group, topic)
		if err != nil {
			return nil, err
		}
		if len(offsets.Offsets) > 0 {
			result = append(result, group)
		}
	}
	return result, nil
}

// getJSON GETs url from the Kafka REST proxy and decodes a 200 answer into out.
func (t *OffsetTracker) getJSON(ctx context.Context, url string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("REST proxy returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// CheckAllConsumersCaughtUp checks if all consumers are caught up to target offset.
// An error means the answer is unknown — the consumer list or a consumer's progress
// could not be read — and the result is false; the map holds the progress that was read.
func (t *OffsetTracker) CheckAllConsumersCaughtUp(
	ctx context.Context,
	topic string,
	targetOffset int64,
	expectedConsumers []string,
) (bool, map[string]*ConsumerProgress, error) {
	progressMap := make(map[string]*ConsumerProgress)

	// Get all consumer groups if none expected
	consumers := expectedConsumers
	if len(consumers) == 0 {
		var err error
		consumers, err = t.GetConsumerGroupsForTopic(ctx, topic)
		if err != nil {
			return false, progressMap, err
		}
		if len(consumers) == 0 {
			return false, progressMap, nil
		}
	}

	allCaughtUp := true
	var errs []error
	for _, consumer := range consumers {
		progress, err := t.GetConsumerProgress(ctx, consumer, topic, targetOffset)
		if err != nil {
			errs = append(errs, err)
			allCaughtUp = false
			continue
		}

		progressMap[consumer] = progress
		if !progress.CaughtUp {
			allCaughtUp = false
		}
	}

	return allCaughtUp, progressMap, errors.Join(errs...)
}

// GetBulkLoadProgress calculates overall progress for a bulk load. An error means a
// consumer's progress could not be read; the progress returned covers the ones that were.
func (t *OffsetTracker) GetBulkLoadProgress(
	ctx context.Context,
	bulkLoad *BulkLoadInfo,
) (float64, map[string]*ConsumerProgress, error) {
	_, progressMap, err := t.CheckAllConsumersCaughtUp(
		ctx,
		bulkLoad.Topic,
		bulkLoad.EndOffset,
		bulkLoad.ExpectedConsumers,
	)

	if len(progressMap) == 0 {
		return 0, progressMap, err
	}

	// Calculate average progress
	var totalProgress float64
	for _, p := range progressMap {
		totalProgress += p.Progress
	}

	return totalProgress / float64(len(progressMap)), progressMap, err
}

// Ensure imports are used
var _ = bytes.Buffer{}
var _ = io.EOF
var _ = sort.Strings
