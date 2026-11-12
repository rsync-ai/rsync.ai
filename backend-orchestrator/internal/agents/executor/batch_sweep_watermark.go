package executor

import (
	"strconv"
	"time"
)

// Which watermark a batch sweep keeps (KI-DB-BATCH-PAGING-CAN-SKIP-ROWS
// cause 3, INCREMENTAL.md §2).
//
// A sweep pages by key, and the value it keeps becomes the next sweep's
// `since`. Keeping the last page's max(updated_at) lost rows: a row the
// cursor had already passed could be updated below a later page's max and
// was then outside both this sweep and the next one's `updated_at > since`.
//
// A database connector now reads max(updated_at) once before the sweep's
// first page and reports it with `snapshot: true`. Every row updated after
// that read is above it, so the next sweep finds it whatever its key. The
// snapshot is kept for the whole sweep, including a resumed half. Pages from
// a connector that cannot take a snapshot still fold to their overall max,
// never below the since the sweep started with, so the watermark cannot move
// backwards.

func watermarkIsSnapshot(wm map[string]interface{}) bool {
	snap, _ := wm["snapshot"].(bool)
	return snap
}

// withWatermarkAtLeast returns wm with its value raised to floor, copying the
// map only when the value changes. Non-string values are left alone: the
// resume path only replays string watermarks as `since`.
func withWatermarkAtLeast(wm map[string]interface{}, floor string) map[string]interface{} {
	value, ok := wm["value"].(string)
	if !ok {
		return wm
	}
	raised := maxWatermarkValue(floor, value)
	if raised == value {
		return wm
	}
	out := make(map[string]interface{}, len(wm))
	for k, v := range wm {
		out[k] = v
	}
	out["value"] = raised
	return out
}

// foldSweepWatermark returns the watermark to keep after one more page.
func foldSweepWatermark(kept, page map[string]interface{}, since string) map[string]interface{} {
	if len(page) == 0 {
		return kept
	}
	if watermarkIsSnapshot(kept) {
		return kept
	}
	if watermarkIsSnapshot(page) {
		return withWatermarkAtLeast(page, since)
	}
	floor := since
	if value, ok := kept["value"].(string); ok {
		floor = maxWatermarkValue(floor, value)
	}
	return withWatermarkAtLeast(page, floor)
}

// seedSweepWatermark is the watermark a resumed table starts from: the one
// its checkpoint holds when this dispatch continues a sweep, none when it
// starts a new one (lastSavedPosition is only set for a continuation).
func seedSweepWatermark(continued map[string]interface{}) map[string]interface{} {
	if wm, ok := continued["watermark"].(map[string]interface{}); ok && len(wm) > 0 {
		return wm
	}
	return nil
}

// The spellings connectors emit: Python isoformat() (aware or naive, `T` or
// space) and a bare date.
var watermarkTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02",
}

func parseWatermarkTime(s string) (time.Time, bool) {
	for _, layout := range watermarkTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// maxWatermarkValue returns the later of two watermark values, compared as
// timestamps, then as numbers. Values it cannot compare return b, the newer
// report, which is what the executor kept before.
func maxWatermarkValue(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if ta, ok := parseWatermarkTime(a); ok {
		if tb, ok := parseWatermarkTime(b); ok {
			if ta.After(tb) {
				return a
			}
			return b
		}
	}
	if fa, err := strconv.ParseFloat(a, 64); err == nil {
		if fb, err := strconv.ParseFloat(b, 64); err == nil {
			if fa > fb {
				return a
			}
			return b
		}
	}
	return b
}
