/**
 * The Runs tab's charts count the runs the tab loads, per UTC day, the way the
 * gateway counts Home's (activity_metrics.go), so a pipeline's charts and Home's
 * agree about the same runs.
 */
import { describe, expect, it } from "vitest"

import { pipelineRunMetrics, type LoadedRun } from "@/components/pipeline/pipeline-run-metrics"

const NOW = Date.parse("2026-10-07T12:00:00Z")
let n = 0
const run = (start: string, status: string, over: Partial<LoadedRun> = {}): LoadedRun => ({
  id: `r${++n}`,
  pipeline_id: "p1",
  status,
  start_time: start,
  ...over,
})

describe("a pipeline's runs per day", () => {
  it("has every day of the window on the axis, ending today", () => {
    const m = pipelineRunMetrics([], 7, { now: NOW })
    expect(m.days).toEqual(["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07"])
    expect(m.pipeline_runs).toEqual([])
    expect(m.since).toBeUndefined()
  })

  it("counts runs by UTC day and outcome, in the outcomes' order", () => {
    const m = pipelineRunMetrics(
      [
        run("2026-10-07T01:00:00Z", "failed"),
        run("2026-10-07T00:30:00Z", "completed"),
        run("2026-10-06T23:59:00Z", "success"),
        run("2026-10-06T10:00:00Z", "silent_drop_detected"),
        run("2026-10-06T09:00:00Z", "pending"),
      ],
      7,
      { now: NOW },
    )
    expect(m.pipeline_runs).toEqual([
      { day: "2026-10-06", status: "succeeded", count: 1 },
      { day: "2026-10-06", status: "failed", count: 1 },
      { day: "2026-10-06", status: "running", count: 1 },
      { day: "2026-10-07", status: "succeeded", count: 1 },
      { day: "2026-10-07", status: "failed", count: 1 },
    ])
  })

  it("leaves out the CDC stream's own row and runs outside the window", () => {
    const m = pipelineRunMetrics(
      [run("2026-10-07T01:00:00Z", "running", { id: "p1" }), run("2026-09-01T01:00:00Z", "completed")],
      7,
      { now: NOW },
    )
    expect(m.pipeline_runs).toEqual([])
    expect(m.rows_written).toEqual([])
  })

  it("times only succeeded and failed runs with an end: median and 95th percentile as Postgres does", () => {
    const at = (secs: number, status = "completed") =>
      run("2026-10-07T01:00:00Z", status, { end_time: new Date(Date.parse("2026-10-07T01:00:00Z") + secs * 1000).toISOString() })
    const m = pipelineRunMetrics(
      [at(10), at(20), at(30), at(40, "failed"), at(999, "cancelled"), run("2026-10-07T01:00:00Z", "completed")],
      7,
      { now: NOW },
    )
    // percentile_cont over 10, 20, 30, 40: median 25, p95 at 2.85 → 38.5.
    expect(m.pipeline_run_time).toEqual([{ day: "2026-10-07", runs: 4, median_seconds: 25, p95_seconds: 38.5 }])
  })

  it("sums the rows every run wrote, a day with runs and none written included", () => {
    const m = pipelineRunMetrics(
      [
        run("2026-10-07T01:00:00Z", "completed", { metrics: { records_processed: 1200 } }),
        run("2026-10-07T02:00:00Z", "failed", { metrics: { records_processed: 30 } }),
        run("2026-10-06T02:00:00Z", "completed", { metrics: null }),
      ],
      7,
      { now: NOW },
    )
    expect(m.rows_written).toEqual([
      { day: "2026-10-06", rows: 0 },
      { day: "2026-10-07", rows: 1230 },
    ])
  })

  it("starts the axis at the oldest loaded run when the list was cut short of the window", () => {
    const runs = [run("2026-10-07T01:00:00Z", "completed"), run("2026-10-05T08:00:00Z", "completed")]
    const cut = pipelineRunMetrics(runs, 7, { now: NOW, truncated: true })
    expect(cut.since).toBe("2026-10-05")
    expect(cut.days).toEqual(["2026-10-05", "2026-10-06", "2026-10-07"])
    // Control: the same runs, all of them loaded, keep the whole window.
    const whole = pipelineRunMetrics(runs, 7, { now: NOW })
    expect(whole.since).toBeUndefined()
    expect(whole.days).toHaveLength(7)
  })
})
