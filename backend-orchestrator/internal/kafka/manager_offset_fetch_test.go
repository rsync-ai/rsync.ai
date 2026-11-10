package kafka

import (
	"errors"
	"reflect"
	"testing"

	"github.com/IBM/sarama"
)

// Bug class: an offset the broker did not give is read as "never committed" (-1).
//
// While a group coordinator is unavailable — a broker restart, prod 2026-10-01 — an
// OffsetFetch answers with an error instead of offsets: on v0-1 (what
// fetchConsumerGroupOffsets sends) the error is repeated on every partition with
// offset -1, on v2+ it is top-level. Read as -1, every partition the sink group did
// commit looks never-committed, so computeConsumerGroupLag drops it: the sink-lag
// tick records lag 0 and a fallen Committed, and cdcstats stops continuing from its
// stored counts. The same misreading in the sink worker restarted two healthy sinks.
// Every caller already treats an error as "no reading this tick".
func TestCommittedFromFetch_RefusesAnUnreadableReading(t *testing.T) {
	requested := map[string][]int32{"dbz.public.matches": {0}, "foreign.other_pipeline": {0}}

	v1CoordinatorError := &sarama.OffsetFetchResponse{Version: 1}
	for topic, parts := range requested {
		for _, p := range parts {
			v1CoordinatorError.AddBlock(topic, p, &sarama.OffsetFetchResponseBlock{Offset: -1, Err: sarama.ErrNotCoordinatorForConsumer})
		}
	}
	oneMissing := &sarama.OffsetFetchResponse{Version: 1}
	oneMissing.AddBlock("foreign.other_pipeline", 0, &sarama.OffsetFetchResponseBlock{Offset: -1})

	cases := []struct {
		name    string
		resp    *sarama.OffsetFetchResponse
		wantErr error // matched with errors.Is when non-nil
	}{
		{name: "v0-1: coordinator error repeated on every partition", resp: v1CoordinatorError, wantErr: sarama.ErrNotCoordinatorForConsumer},
		{name: "v2+: coordinator still loading, top-level error", resp: &sarama.OffsetFetchResponse{Version: 2, Err: sarama.ErrOffsetsLoadInProgress}, wantErr: sarama.ErrOffsetsLoadInProgress},
		{name: "requested partition missing from the answer", resp: oneMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			committed, err := committedFromFetch(requested, tc.resp)
			if err == nil {
				t.Fatalf("no error on a reading the broker did not give; it would read as committed=%v, Committed=%d",
					committed, sumCommittedOffsets(committed))
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v does not wrap the broker's %v", err, tc.wantErr)
			}
		})
	}
}

// The control: a partition the group never committed answers -1 with no error, and
// stays -1 — that is how computeConsumerGroupLag scopes a group to its own topics.
func TestCommittedFromFetch_KeepsNeverCommittedAsMinusOne(t *testing.T) {
	requested := map[string][]int32{"dbz.public.matches": {0, 1}, "foreign.other_pipeline": {0}}
	resp := &sarama.OffsetFetchResponse{Version: 1}
	resp.AddBlock("dbz.public.matches", 0, &sarama.OffsetFetchResponseBlock{Offset: 4005})
	resp.AddBlock("dbz.public.matches", 1, &sarama.OffsetFetchResponseBlock{Offset: 17})
	resp.AddBlock("foreign.other_pipeline", 0, &sarama.OffsetFetchResponseBlock{Offset: -1})

	committed, err := committedFromFetch(requested, resp)
	if err != nil {
		t.Fatalf("committedFromFetch: %v", err)
	}
	want := map[string]map[int32]int64{
		"dbz.public.matches":     {0: 4005, 1: 17},
		"foreign.other_pipeline": {0: -1},
	}
	if !reflect.DeepEqual(committed, want) {
		t.Fatalf("committed=%v, want %v", committed, want)
	}
	if got := sumCommittedOffsets(committed); got != 4022 {
		t.Fatalf("Committed=%d, want 4022 (the -1 partition ignored)", got)
	}
}
