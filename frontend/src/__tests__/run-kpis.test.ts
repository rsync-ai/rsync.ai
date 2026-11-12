import { describe, expect, it } from "vitest"

import { compactCount, runKpis, successRate } from "@/components/runs/run-kpis"
import type { ActivityMetrics } from "@/lib/api/activity-metrics"

// The four numbers above the Runs list. They pin which runs count, that the
// window is the last 7 days of whatever the server sent, and that a success rate
// never rounds a failure away.

describe("successRate", () => {
  it.each([
    [0, 0, null],
    [10, 0, "100%"],
    [84, 1, "98.8%"],
    [1, 1, "50%"],
    [0, 3, "0%"],
    // 99.99% would round to 100% and hide the one failure.
    [9999, 1, "99.9%"],
  ])("%i succeeded, %i failed → %s", (ok, failed, want) => {
    expect(successRate(ok, failed)).toBe(want)
  })
})

describe("compactCount", () => {
  it.each([
    [0, "0"],
    [940, "940"],
    [12_000, "12K"],
    [3_412_551, "3.4M"],
  ])("%i → %s", (n, want) => {
    expect(compactCount(n)).toBe(want)
  })
})

describe("runKpis", () => {
  const days = ["d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9"]
  const metrics: ActivityMetrics = {
    days,
    time_zone: "UTC",
    pipeline_runs: [
      // d1 and d2 are outside the last 7 days and must not count.
      { day: "d1", status: "failed", count: 50 },
      { day: "d2", status: "succeeded", count: 50 },
      { day: "d3", status: "succeeded", count: 4 },
      { day: "d3", status: "failed", count: 1 },
      { day: "d5", status: "cancelled", count: 2 },
      { day: "d9", status: "running", count: 1 },
    ],
    model_runs: [
      { day: "d4", status: "succeeded", count: 3 },
      { day: "d9", status: "skipped", count: 5 },
      { day: "d9", status: "failed", count: 1 },
    ],
    rows_written: [
      { day: "d2", rows: 1_000_000 },
      { day: "d3", rows: 1_200 },
      { day: "d8", rows: 300 },
    ],
    pipeline_run_time: [],
    model_run_time: [],
    checks: [],
  }

  it("adds pipeline and model runs over the last 7 days, every outcome", () => {
    const k = runKpis(metrics)
    expect(k.runs).toBe(4 + 1 + 2 + 1 + 3 + 5 + 1)
    expect(k.failed).toBe(2)
    // 7 succeeded, 2 failed; cancelled, skipped and running are neither.
    expect(k.successRate).toBe("77.7%")
    expect(k.rowsWritten).toBe(1_500)
  })

  it("gives each card its per-day line, oldest first, a quiet day as 0", () => {
    const k = runKpis(metrics)
    expect(k.runsByDay).toEqual([5, 3, 2, 0, 0, 0, 7])
    expect(k.failedByDay).toEqual([1, 0, 0, 0, 0, 0, 1])
    expect(k.rowsByDay).toEqual([1_200, 0, 0, 0, 0, 300, 0])
  })

  it("has no success rate when nothing succeeded or failed", () => {
    const k = runKpis({ ...metrics, pipeline_runs: [{ day: "d9", status: "running", count: 2 }], model_runs: [] })
    expect(k.runs).toBe(2)
    expect(k.successRate).toBeNull()
  })
})
