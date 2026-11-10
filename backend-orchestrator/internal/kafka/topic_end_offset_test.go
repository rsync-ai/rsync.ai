package kafka

import (
	"testing"

	"github.com/IBM/sarama"
)

// endOffsetClient serves a fixed topic list and per-partition end offsets, and counts
// metadata requests so a test can prove a missing topic was never asked about.
type endOffsetClient struct {
	sarama.Client
	topics          []string
	ends            map[int32]int64
	partitionsCalls int
}

func (c *endOffsetClient) Topics() ([]string, error) { return c.topics, nil }

func (c *endOffsetClient) Partitions(string) ([]int32, error) {
	c.partitionsCalls++
	out := make([]int32, 0, len(c.ends))
	for p := range c.ends {
		out = append(out, p)
	}
	return out, nil
}

func (c *endOffsetClient) GetOffset(_ string, p int32, at int64) (int64, error) {
	if at != sarama.OffsetNewest {
		panic("TopicEndOffset must read only the log end offset")
	}
	return c.ends[p], nil
}

// The end offset is summed over partitions, so a heartbeat landing on any partition
// moves it.
func TestTopicEndOffsetSumsEveryPartition(t *testing.T) {
	c := &endOffsetClient{topics: []string{"other", "__debezium-heartbeat.cdc-1"}, ends: map[int32]int64{0: 7, 1: 5}}
	m := &Manager{connected: true, client: c}

	end, exists, err := m.TopicEndOffset("__debezium-heartbeat.cdc-1")
	if err != nil || !exists || end != 12 {
		t.Fatalf("TopicEndOffset = (%d, %v, %v), want (12, true, nil)", end, exists, err)
	}
}

// A watchdog must not create the topic it watches: a metadata request for a missing topic
// auto-creates it on a broker with auto.create.topics.enable, so a topic the broker does
// not list is reported absent without one.
func TestTopicEndOffsetNeverAsksAboutAMissingTopic(t *testing.T) {
	c := &endOffsetClient{topics: []string{"other"}, ends: map[int32]int64{0: 7}}
	m := &Manager{connected: true, client: c}

	end, exists, err := m.TopicEndOffset("__debezium-heartbeat.cdc-1")
	if err != nil || exists || end != 0 {
		t.Fatalf("TopicEndOffset = (%d, %v, %v), want (0, false, nil)", end, exists, err)
	}
	if c.partitionsCalls != 0 {
		t.Fatalf("a missing topic was asked for its partitions %d time(s): that request can create it", c.partitionsCalls)
	}
}

func TestTopicEndOffsetNeedsAConnection(t *testing.T) {
	m := &Manager{}
	if _, _, err := m.TopicEndOffset("t"); err == nil {
		t.Fatal("a disconnected manager answered TopicEndOffset without an error")
	}
}
