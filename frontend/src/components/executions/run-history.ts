/**
 * A run among its pipeline's recent runs, as the trends endpoint lists them
 * (pipeline_compare.go, newest first): the run it is compared against, and how
 * its duration changed. Run detail renders on the server and reads these there.
 */

import type { ChartRun } from "@/components/runs/run-chart-specs"

const FINISHED = new Set(["completed", "success", "failed"])

/**
 * The run to compare this one against: the newest finished run older than it.
 * Nothing when this run is not in the list: a newer run is not its previous run.
 * The CDC stream's id is the pipeline id, and a run still in flight has no
 * duration or outcome; comparing against either rendered "— → —".
 */
export function pickPreviousRun<T extends Pick<ChartRun, "execution_id"> & { status?: string }>(
  recent: readonly T[],
  executionId: string,
  pipelineId: string,
): T | undefined {
  const idx = recent.findIndex((e) => e?.execution_id === executionId)
  if (idx < 0) return undefined
  return recent
    .slice(idx + 1)
    .find(
      (e) =>
        e?.execution_id &&
        e.execution_id !== pipelineId &&
        FINISHED.has(String(e.status || "").toLowerCase()),
    )
}

/**
 * "8% faster than the previous run", from both runs' durations in the list.
 * Nothing when either has no duration (it never finished) or there is no
 * previous run.
 */
export function durationChange(recent: readonly ChartRun[], executionId: string, pipelineId: string): string | undefined {
  const run = recent.find((e) => e.execution_id === executionId)
  const prev = pickPreviousRun(recent, executionId, pipelineId)
  const now = run?.duration_ms
  const before = prev?.duration_ms
  if (typeof now !== "number" || typeof before !== "number" || before <= 0) return undefined
  const pct = Math.round(((now - before) / before) * 100)
  if (pct === 0) return "About as long as the previous run"
  return `${Math.abs(pct)}% ${pct < 0 ? "faster" : "slower"} than the previous run`
}
