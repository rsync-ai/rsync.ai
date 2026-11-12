package executor

import (
	"strings"
	"testing"
)

// KI-DB-BATCH-PAGING-CAN-SKIP-ROWS cause (3): a batch sweep pages by key, but
// its watermark is a timestamp. Whatever the sweep keeps as its watermark is
// the `since` of the next sweep, so keeping one that is too high loses every
// row updated below it; one lower than the last `since` re-reads rows for no
// reason. The two tests below are the two ways the kept value was wrong.

func wmPage(value string, snapshot bool) map[string]interface{} {
	page := map[string]interface{}{"field": "updated_at", "value": value}
	if snapshot {
		page["snapshot"] = true
	}
	return page
}

func foldPages(since string, seed map[string]interface{}, pages ...map[string]interface{}) map[string]interface{} {
	kept := seed
	for _, page := range pages {
		kept = foldSweepWatermark(kept, page, since)
	}
	return kept
}

// (3a) Row 1 is read on page 1. While the sweep is still reading, row 1 is
// updated at 10:04 and row 900 at 10:05; page 2 reads row 900 and reports
// 10:05. Keeping 10:05 means the next sweep asks for updated_at > 10:05 and
// never sees row 1's update. The sweep-start snapshot (10:00) is the only
// value below every update the sweep might have missed.
func TestSweepWatermarkKeepsTheSweepStartSnapshot(t *testing.T) {
	kept := foldPages("2026-09-29T09:00:00",
		nil,
		wmPage("2026-09-29T10:00:00", true),
		wmPage("2026-09-29T10:05:00", false),
	)
	got, _ := kept["value"].(string)
	if got != "2026-09-29T10:00:00" {
		t.Fatalf("kept watermark = %q, want the sweep-start snapshot 2026-09-29T10:00:00: row 1 updated at 10:04 behind the paging cursor would be skipped by the next sweep", got)
	}
	if snap, _ := kept["snapshot"].(bool); !snap {
		t.Fatalf("kept watermark lost its snapshot flag: %v", kept)
	}
}

// (3b) Without a snapshot (an older connector image, or a dialect that cannot
// take one) pages still report their own max. Keys and timestamps are not
// ordered together, so the last page's max can be lower than an earlier
// page's, or lower than the since the sweep started with.
func TestSweepWatermarkNeverRegresses(t *testing.T) {
	cases := []struct {
		name  string
		since string
		pages []map[string]interface{}
		want  string
	}{
		{"last page lower than an earlier page", "2026-09-29T09:00:00",
			[]map[string]interface{}{wmPage("2026-09-29T10:30:00", false), wmPage("2026-09-29T10:10:00", false)},
			"2026-09-29T10:30:00"},
		{"every page lower than since", "2026-09-29T11:00:00",
			[]map[string]interface{}{wmPage("2026-09-29T10:30:00", false)},
			"2026-09-29T11:00:00"},
		{"snapshot lower than since", "2026-09-29T11:00:00",
			[]map[string]interface{}{wmPage("2026-09-29T10:30:00", true)},
			"2026-09-29T11:00:00"},
		{"aware values compare as instants", "",
			[]map[string]interface{}{wmPage("2026-09-29T12:00:00+05:30", false), wmPage("2026-09-29T07:00:00Z", false)},
			"2026-09-29T07:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept := foldPages(tc.since, nil, tc.pages...)
			if got, _ := kept["value"].(string); got != tc.want {
				t.Fatalf("kept watermark = %q, want %q", got, tc.want)
			}
		})
	}
}

// A sweep split across dispatches (chunking, a worker restart) resumes with
// the watermark its checkpoint holds. Starting from nothing would let the
// resumed half's pages replace the first half's snapshot.
func TestSweepWatermarkResumesFromTheCheckpoint(t *testing.T) {
	continued := map[string]interface{}{
		"batch_idx": float64(3),
		"watermark": map[string]interface{}{"field": "updated_at", "value": "2026-09-29T10:00:00", "snapshot": true},
	}
	kept := foldPages("2026-09-29T09:00:00", seedSweepWatermark(continued), wmPage("2026-09-29T10:05:00", false))
	if got, _ := kept["value"].(string); got != "2026-09-29T10:00:00" {
		t.Fatalf("resumed sweep kept %q, want its checkpointed snapshot 2026-09-29T10:00:00", got)
	}
	if seedSweepWatermark(nil) != nil {
		t.Fatal("a new sweep must start with no watermark")
	}
	if seedSweepWatermark(map[string]interface{}{"batch_idx": float64(3)}) != nil {
		t.Fatal("a checkpoint without a watermark must seed none")
	}
}

func TestMaxWatermarkValue(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"2026-09-29T10:00:00", "2026-09-29T09:00:00", "2026-09-29T10:00:00"},
		{"2026-09-29T09:00:00", "2026-09-29T10:00:00", "2026-09-29T10:00:00"},
		{"2026-09-29 10:00:00.5", "2026-09-29 10:00:00.25", "2026-09-29 10:00:00.5"},
		{"2026-09-29T10:00:00.123456", "2026-09-29T10:00:00.1234", "2026-09-29T10:00:00.123456"},
		{"2026-09-30", "2026-09-29T23:59:59", "2026-09-30"},
		{"2026-09-29T10:00:00+00:00", "2026-09-29T11:00:00+02:00", "2026-09-29T10:00:00+00:00"},
		{"9", "10", "10"},
		{"10.5", "2", "10.5"},
		{"", "2026-09-29T10:00:00", "2026-09-29T10:00:00"},
		{"2026-09-29T10:00:00", "", "2026-09-29T10:00:00"},
		// Incomparable: keep the newer report, as the executor always did.
		{"abc", "2026-09-29T10:00:00", "2026-09-29T10:00:00"},
	}
	for _, tc := range cases {
		if got := maxWatermarkValue(tc.a, tc.b); got != tc.want {
			t.Errorf("maxWatermarkValue(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// The helpers only matter if executeBatchDataTransfer uses them: a page's
// watermark goes through the fold, and the fold starts from the checkpoint.
func TestSweepWatermarkIsWired(t *testing.T) {
	batch := executeBatchDataTransferBody(t)
	for _, want := range []string{
		"currentWatermark := seedSweepWatermark(lastSavedPosition)",
		"currentWatermark = foldSweepWatermark(currentWatermark, wm, incrementalSince)",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("executeBatchDataTransfer no longer contains %q", want)
		}
	}
	if strings.Contains(batch, "currentWatermark = wm") {
		t.Error("executeBatchDataTransfer overwrites the watermark with a single page's again")
	}
}
