/**
 * Prod retest 2026-09-19: "Comparison with previous run" for run 402b1e5e
 * compared it against 67b8ac8b, the pipeline id that a CDC pipeline's stream
 * stats carry as their execution id, and showed "— → —".
 *
 * Launch review PR2: the comparison is now the Duration card's change on Run
 * detail, and a run outside the list has no previous run. It used to take the
 * newest finished run, which is newer than it: "faster than the previous run"
 * against a run that came after it.
 */
import { describe, it, expect } from "vitest"
import { durationChange, pickPreviousRun } from "@/components/executions/run-history"

const PIPELINE = "67b8ac8b-0000-4000-8000-000000000001"

describe("the run a run is compared against", () => {
  it("skips the CDC stream id and runs still in flight", () => {
    const recent = [
      { execution_id: PIPELINE, status: "running" },
      { execution_id: "run-3", status: "running" },
      { execution_id: "run-2", status: "running" },
      { execution_id: "run-1", status: "completed" },
    ]
    expect(pickPreviousRun(recent, "run-3", PIPELINE)?.execution_id).toBe("run-1")
  })

  it("is older than this run, never newer", () => {
    const recent = [
      { execution_id: "run-3", status: "completed" },
      { execution_id: "run-2", status: "failed" },
      { execution_id: "run-1", status: "completed" },
    ]
    expect(pickPreviousRun(recent, "run-2", PIPELINE)?.execution_id).toBe("run-1")
  })

  it("is nothing when this run is outside the list, since every run in it is newer", () => {
    const recent = [
      { execution_id: PIPELINE, status: "running" },
      { execution_id: "run-9", status: "failed" },
    ]
    expect(pickPreviousRun(recent, "run-1", PIPELINE)).toBeUndefined()
  })

  it("control: nothing finished before it means no comparison", () => {
    const recent = [
      { execution_id: "run-2", status: "completed" },
      { execution_id: PIPELINE, status: "running" },
    ]
    expect(pickPreviousRun(recent, "run-2", PIPELINE)).toBeUndefined()
  })
})

describe("how a run's duration changed", () => {
  const recent = [
    { execution_id: "run-3", status: "completed", duration_ms: 18_400 },
    { execution_id: "run-2", status: "completed", duration_ms: 20_000 },
    { execution_id: "run-1", status: "completed", duration_ms: 20_000 },
  ]

  it("is faster or slower than the previous run, in whole percent", () => {
    expect(durationChange(recent, "run-3", PIPELINE)).toBe("8% faster than the previous run")
    expect(durationChange([{ ...recent[0], duration_ms: 25_000 }, recent[1]], "run-3", PIPELINE)).toBe(
      "25% slower than the previous run",
    )
    expect(durationChange(recent, "run-2", PIPELINE)).toBe("About as long as the previous run")
  })

  it("is nothing for a run with no duration, or with no previous run", () => {
    expect(durationChange([{ execution_id: "run-3", status: "cancelled" }, recent[1]], "run-3", PIPELINE)).toBeUndefined()
    expect(durationChange(recent, "run-1", PIPELINE)).toBeUndefined()
    expect(durationChange(recent, "run-0", PIPELINE)).toBeUndefined()
  })
})
