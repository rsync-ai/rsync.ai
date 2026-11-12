package handlers

import (
	"database/sql"
	"testing"
	"time"
)

// The rule a policy='all' fan-in uses to decide whether one upstream has rebuilt since
// this model's last build. The SQL that gathers the facts is covered against real
// Postgres in cdc_watermark_pg_test.go; this pins the decision in a lane CI runs.
//
// Bug class (KI-CDC-UPSTREAM-ALL-NEVER-FRESH): a CDC upstream judged by a batch-only
// completion signal. A CDC pipeline never emits PIPELINE_COMPLETED, so a rule that
// only reads completions holds an 'all' model forever once its CDC source goes idle.
func TestUpstreamIsFresh(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) sql.NullTime { return sql.NullTime{Time: base.Add(d), Valid: true} }
	baseline := at(0)
	never := sql.NullTime{}

	for _, tc := range []struct {
		name     string
		facts    upstreamFacts
		baseline sql.NullTime
		want     bool
	}{
		{"model rebuilt after the baseline", upstreamFacts{IsModel: true, ModelSucceededAt: at(time.Minute)}, baseline, true},
		{"model last rebuilt before the baseline", upstreamFacts{IsModel: true, ModelSucceededAt: at(-time.Minute)}, baseline, false},
		{"model rebuilt at the baseline instant is not after it", upstreamFacts{IsModel: true, ModelSucceededAt: at(0)}, baseline, false},
		{"model never rebuilt", upstreamFacts{IsModel: true}, baseline, false},
		{"no build of this model yet: any rebuild counts", upstreamFacts{IsModel: true, ModelSucceededAt: at(-time.Hour)}, never, true},
		{"a model upstream ignores pipeline facts",
			upstreamFacts{IsModel: true, PipelineCompletedAt: at(time.Minute), IsCDC: true, WatermarkSourceTime: at(time.Minute)}, baseline, false},

		{"batch pipeline completed after the baseline", upstreamFacts{PipelineCompletedAt: at(time.Minute)}, baseline, true},
		{"batch pipeline last completed before the baseline", upstreamFacts{PipelineCompletedAt: at(-time.Minute)}, baseline, false},
		{"batch pipeline is never fresh by sink lag or watermark",
			upstreamFacts{SinkCaughtUpAt: at(time.Minute), WatermarkSourceTime: at(time.Minute)}, baseline, false},

		{"CDC: sink drained after the baseline", upstreamFacts{IsCDC: true, SinkCaughtUpAt: at(time.Minute)}, baseline, true},
		{"CDC: watermark past the baseline", upstreamFacts{IsCDC: true, WatermarkSourceTime: at(time.Minute)}, baseline, true},
		{"CDC: a completion still counts", upstreamFacts{IsCDC: true, PipelineCompletedAt: at(time.Minute)}, baseline, true},
		{"CDC: sink last drained before the baseline", upstreamFacts{IsCDC: true, SinkCaughtUpAt: at(-time.Minute)}, baseline, false},
		{"CDC: watermark older than the baseline", upstreamFacts{IsCDC: true, WatermarkSourceTime: at(-time.Minute)}, baseline, false},
		{"CDC: no reading at all", upstreamFacts{IsCDC: true}, baseline, false},
		{"CDC: no build of this model yet, any drained reading counts", upstreamFacts{IsCDC: true, SinkCaughtUpAt: at(-time.Hour)}, never, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamIsFresh(tc.facts, tc.baseline); got != tc.want {
				t.Fatalf("upstreamIsFresh = %v, want %v", got, tc.want)
			}
		})
	}
}
