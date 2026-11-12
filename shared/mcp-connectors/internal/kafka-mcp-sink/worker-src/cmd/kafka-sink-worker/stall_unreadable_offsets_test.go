package main

import (
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
)

// Bug class: a broker reading the watchdog could not take is read as a real offset.
//
// Prod, 2026-10-01 12:11 UTC: the broker was recreated, and while its group
// coordinator was still loading, OffsetFetch answered with an error instead of the
// offsets. The watchdog read "no offset" as "never committed" (committed=-1), set the
// floor to the first offset, saw end=4005 > 0, and restarted two healthy CDC sink
// workers whose groups had committed everything. A reading it cannot take must make
// waitingRecords return an error — watchConsumerStall already treats an error as
// "cannot see the broker, not restarting" — never a decision.
func TestWaitingRecordsRefusesToDecideOnAnUnreadableReading(t *testing.T) {
	const topic = "rsync.cdc.public.matches"
	end := kafka.PartitionOffsets{Partition: 0, FirstOffset: 0, LastOffset: 4005}

	cases := []struct {
		name      string
		offsets   *kafka.ListOffsetsResponse
		committed *kafka.OffsetFetchResponse
		wantErr   error // matched with errors.Is when non-nil
	}{
		{
			// OffsetFetch v2+: a coordinator error is top-level and the partitions are omitted.
			name:      "coordinator still loading: top-level error, no partitions",
			offsets:   offsetsOf(topic, end),
			committed: &kafka.OffsetFetchResponse{Topics: map[string][]kafka.OffsetFetchPartition{}, Error: kafka.GroupLoadInProgress},
			wantErr:   kafka.GroupLoadInProgress,
		},
		{
			// OffsetFetch v0-1: the same error is repeated on every partition, offset -1.
			name:    "coordinator moved: per-partition error with offset -1",
			offsets: offsetsOf(topic, end),
			committed: committedOf(topic, kafka.OffsetFetchPartition{
				Partition: 0, CommittedOffset: -1, Error: kafka.NotCoordinatorForGroup}),
			wantErr: kafka.NotCoordinatorForGroup,
		},
		{
			name:      "partition missing from the offset-fetch answer, no error anywhere",
			offsets:   offsetsOf(topic, end),
			committed: &kafka.OffsetFetchResponse{Topics: map[string][]kafka.OffsetFetchPartition{}},
		},
		{
			// kafka-go leaves the end offset at -1 here; recording that as the baseline
			// would make every pre-existing record look waiting to a `latest` reader.
			name: "end offset unreadable: list-offsets partition error",
			offsets: offsetsOf(topic, kafka.PartitionOffsets{
				Partition: 0, FirstOffset: -1, LastOffset: -1, Error: kafka.LeaderNotAvailable}),
			committed: committedOf(topic, kafka.OffsetFetchPartition{Partition: 0, CommittedOffset: 4005}),
			wantErr:   kafka.LeaderNotAvailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := map[string]int64{}
			waiting, detail, err := waitingRecords(tc.offsets, tc.committed, true, baseline, nil)
			if err == nil {
				t.Fatalf("waitingRecords decided waiting=%v (%q) on a reading it could not take; want an error so the watchdog does not restart", waiting, detail)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v does not wrap the broker's %v", err, tc.wantErr)
			}
			if waiting {
				t.Fatalf("waiting=true alongside error %v", err)
			}
			if len(baseline) != 0 {
				t.Fatalf("an unreadable reading seeded the baseline: %v", baseline)
			}
		})
	}
}

// The controls: the fix must not blind the watchdog. A group that never committed
// answers with offset -1 and no error, and that is exactly the empty-assignment wedge
// the watchdog exists to catch.
func TestWaitingRecordsStillDecidesOnAReadableReading(t *testing.T) {
	const topic = "rsync.cdc.public.matches"
	key := topic + "/0"

	t.Run("never committed, reading from the earliest: records waiting", func(t *testing.T) {
		waiting, detail, err := waitingRecords(
			offsetsOf(topic, kafka.PartitionOffsets{Partition: 0, FirstOffset: 0, LastOffset: 3}),
			committedOf(topic, kafka.OffsetFetchPartition{Partition: 0, CommittedOffset: -1}),
			true, map[string]int64{}, nil)
		if err != nil || !waiting {
			t.Fatalf("waiting=%v err=%v, want waiting with no error (the empty-assignment wedge)", waiting, err)
		}
		t.Logf("detail: %s", detail)
	})

	t.Run("committed up to the end: nothing waiting", func(t *testing.T) {
		waiting, _, err := waitingRecords(
			offsetsOf(topic, kafka.PartitionOffsets{Partition: 0, FirstOffset: 0, LastOffset: 4005}),
			committedOf(topic, kafka.OffsetFetchPartition{Partition: 0, CommittedOffset: 4005}),
			true, map[string]int64{}, nil)
		if err != nil || waiting {
			t.Fatalf("waiting=%v err=%v, want nothing waiting", waiting, err)
		}
	})

	t.Run("never committed, reading from the latest: only records past the baseline wait", func(t *testing.T) {
		baseline := map[string]int64{}
		waiting, _, err := waitingRecords(
			offsetsOf(topic, kafka.PartitionOffsets{Partition: 0, FirstOffset: 0, LastOffset: 500}),
			committedOf(topic, kafka.OffsetFetchPartition{Partition: 0, CommittedOffset: -1}),
			false, baseline, nil)
		if err != nil || waiting {
			t.Fatalf("first look: waiting=%v err=%v, want pre-existing records not waiting", waiting, err)
		}
		if baseline[key] != 500 {
			t.Fatalf("baseline=%v, want %s=500", baseline, key)
		}
		waiting, _, err = waitingRecords(
			offsetsOf(topic, kafka.PartitionOffsets{Partition: 0, FirstOffset: 0, LastOffset: 501}),
			committedOf(topic, kafka.OffsetFetchPartition{Partition: 0, CommittedOffset: -1}),
			false, baseline, nil)
		if err != nil || !waiting {
			t.Fatalf("after a new record: waiting=%v err=%v, want waiting", waiting, err)
		}
	})
}

func offsetsOf(topic string, parts ...kafka.PartitionOffsets) *kafka.ListOffsetsResponse {
	return &kafka.ListOffsetsResponse{Topics: map[string][]kafka.PartitionOffsets{topic: parts}}
}

func committedOf(topic string, parts ...kafka.OffsetFetchPartition) *kafka.OffsetFetchResponse {
	return &kafka.OffsetFetchResponse{Topics: map[string][]kafka.OffsetFetchPartition{topic: parts}}
}
