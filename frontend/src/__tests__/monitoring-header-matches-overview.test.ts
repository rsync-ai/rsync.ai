/**
 * Prod retest 2026-09-19 on a CDC pipeline: the Monitoring header said
 * "Step 1/1" and "trace 67b8ac8b" (the pipeline id) beside an Overview that said
 * "Step 2/2" for run 402b1e5e, whose events carry trace 402b1e5e.
 *
 * - The header read state.progress; it now reads the Overview's own timeline.
 * - The header took the newest row's trace, and the newest row of a CDC pipeline
 *   is a stream stat stamped with the pipeline id. That header (step, trace, run
 *   link) has since been removed: the page header and Live Execution say it once.
 *   The step counter below is what Live Execution's timeline counts: its stages
 *   (buildAgenticStagesFromEvents) through deriveStepInfo, as the panel does.
 */
import { describe, it, expect, vi } from "vitest"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}))

import { buildAgenticStagesFromEvents, deriveStepInfo } from "@/components/pipeline/PipelineLiveStatePanel"

type Ev = Parameters<typeof buildAgenticStagesFromEvents>[0][number]
type State = NonNullable<Parameters<typeof buildAgenticStagesFromEvents>[1]>

const stepOf = (events: Ev[], state: State) =>
  deriveStepInfo(buildAgenticStagesFromEvents(events, state), undefined, "running")

const PIPELINE = "67b8ac8b-0000-4000-8000-000000000001"
const RUN = "402b1e5e-0000-4000-8000-000000000002"

let seq = 0
function ev(stage: string, type: string, second: number, exec = RUN): Ev {
  seq += 1
  return {
    pipeline_id: PIPELINE,
    execution_id: exec,
    event_id: `ev-${seq}`,
    event_type: type,
    stage_id: stage,
    occurred_at: new Date(Date.UTC(2026, 8, 19, 12, 26, second)).toISOString(),
    payload: {},
  }
}

const runEvents: Ev[] = [
  ev("infra_preflight", "STAGE_STARTED", 10),
  ev("infra_preflight", "STAGE_COMPLETED", 20),
  ev("executor", "STAGE_STARTED", 25),
  ev("executor", "STAGE_COMPLETED", 59),
]

const state = {
  pipeline_id: PIPELINE,
  execution_id: RUN,
  status: "running",
  trace_id: undefined,
  progress: { current_step: 1, total_steps: 1, stage: "" },
} as unknown as State

describe("the run's step, from its own stages", () => {
  it("counts the stages the Overview's timeline shows, not state.progress", () => {
    const info = stepOf(runEvents, state)
    expect(info).toMatchObject({ current_step: 2, total_steps: 2 })
  })

  it("ignores another execution's stages", () => {
    const other = [ev("planner", "STAGE_STARTED", 1, PIPELINE), ev("planner", "STAGE_COMPLETED", 2, PIPELINE)]
    expect(stepOf([...other, ...runEvents], state)).toMatchObject({ current_step: 2, total_steps: 2 })
  })

  it("control: no stages at all leaves the header to state.progress", () => {
    expect(stepOf([], state)).toBeNull()
    expect(deriveStepInfo([], "executor")).toBeNull()
  })
})
