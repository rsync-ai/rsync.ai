/**
 * One word for each run status (PR1, bug 2 of the launch review).
 *
 * The same succeeded run read "Success" on Run detail and in a pipeline's run
 * history, and "Succeeded" on Runs and Home; a run waiting on the user read
 * "Awaiting User" on Run detail and "Waiting for you" everywhere else. The run
 * history filter offered both "Success" and "Completed", two words for one
 * outcome, because the gateway matches one stored status exactly
 * (`pipelines.go:4285`) and a succeeded run is stored as either.
 *
 * Bug class: a run-status label map that spells a status its own way instead of
 * reading `ACTIVITY_STATUS_LABELS`. Every map is checked against it here.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/pipelines/p1",
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const listExecutionsResponse = vi.fn()
vi.mock("@/lib/api/executions", () => ({
  listExecutionsResponse: (...a: unknown[]) => listExecutionsResponse(...a),
}))
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: async () => ({ ok: false, status: 500, json: async () => ({}) }),
}))
vi.mock("@/lib/api/pipelines", () => ({
  executePipelineWithRunMode: vi.fn(),
  AssessmentRequiredError: class extends Error {},
}))

import { ACTIVITY_STATUS_LABELS } from "@/lib/api/activity"
import { executionStatusConfig } from "@/lib/execution-status"
import { statusPresentation } from "@/components/pipeline/ExecutionDetailDialog"
import { ExecutionHistoryTab, mergeNewestFirst } from "@/components/pipeline/ExecutionHistoryTab"

beforeEach(() => {
  vi.clearAllMocks()
})

describe("every run-status label map uses the shared words", () => {
  it("Run detail says Succeeded and Waiting for you", () => {
    expect(executionStatusConfig.success.label).toBe(ACTIVITY_STATUS_LABELS.succeeded)
    expect(executionStatusConfig.waiting_for_user.label).toBe(ACTIVITY_STATUS_LABELS.waiting)
    expect(executionStatusConfig.running.label).toBe(ACTIVITY_STATUS_LABELS.running)
    expect(executionStatusConfig.failed.label).toBe(ACTIVITY_STATUS_LABELS.failed)
    expect(executionStatusConfig.cancelled.label).toBe(ACTIVITY_STATUS_LABELS.cancelled)
  })

  it("a pipeline's run history says Succeeded for both stored spellings", () => {
    expect(statusPresentation("success").label).toBe("Succeeded")
    expect(statusPresentation("completed").label).toBe("Succeeded")
    expect(statusPresentation("running").label).toBe(ACTIVITY_STATUS_LABELS.running)
    expect(statusPresentation("failed").label).toBe(ACTIVITY_STATUS_LABELS.failed)
    expect(statusPresentation("cancelled").label).toBe(ACTIVITY_STATUS_LABELS.cancelled)
  })
})

function exec(id: string, status: string, start: string) {
  return { id, pipeline_id: "p1", status, start_time: start, end_time: null, error_message: null, metrics: null }
}

describe("mergeNewestFirst", () => {
  it("keeps one list as the server ordered it", () => {
    const one = [exec("b", "success", "2026-10-07T09:00:00Z"), exec("a", "success", "2026-10-07T10:00:00Z")]
    expect(mergeNewestFirst([one], 250).map((e) => e.id)).toEqual(["b", "a"])
  })

  it("interleaves several lists newest first, drops repeats and stops at the limit", () => {
    const success = [exec("s2", "success", "2026-10-07T12:00:00Z"), exec("s1", "success", "2026-10-07T08:00:00Z")]
    const completed = [exec("c1", "completed", "2026-10-07T10:00:00Z"), exec("s2", "completed", "2026-10-07T12:00:00Z")]
    expect(mergeNewestFirst([success, completed], 250).map((e) => e.id)).toEqual(["s2", "c1", "s1"])
    expect(mergeNewestFirst([success, completed], 2).map((e) => e.id)).toEqual(["s2", "c1"])
  })
})

describe("the run history filter has one Succeeded option", () => {
  it("lists no Success or Completed, and Succeeded reads both stored spellings", async () => {
    listExecutionsResponse.mockImplementation(async (p: { status?: string }) => {
      if (p.status === "success") return { executions: [exec("e0000001", "success", "2026-10-07T08:00:00Z")] }
      if (p.status === "completed") return { executions: [exec("e0000002", "completed", "2026-10-07T09:00:00Z")] }
      return { executions: [exec("e0000009", "failed", "2026-10-07T07:00:00Z")] }
    })
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getByText("e0000009...")).toBeInTheDocument())

    await user.click(screen.getByRole("combobox", { name: "Status" }))
    const names = screen.getAllByRole("option").map((o) => o.textContent)
    expect(names).toContain("Succeeded")
    expect(names).not.toContain("Success")
    expect(names).not.toContain("Completed")

    listExecutionsResponse.mockClear()
    await user.click(screen.getByRole("option", { name: "Succeeded" }))

    await waitFor(() => expect(screen.getByText("e0000002...")).toBeInTheDocument())
    expect(listExecutionsResponse.mock.calls.map((c) => c[0].status).sort()).toEqual(["completed", "success"])
    const rows = screen.getAllByRole("row").slice(1)
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["e0000002...", "e0000001..."])
    expect(rows.every((r) => within(r).getByText("Succeeded"))).toBe(true)
  })

  // Succeeded asks twice, so its answer can land after a later filter's answer.
  // Bug class: a list fetch that writes state with no check that it is still
  // the latest request.
  it("drops an answer that lands after a later filter's answer", async () => {
    const held: Array<() => void> = []
    listExecutionsResponse.mockImplementation((p: { status?: string }) => {
      if (p.status === "success" || p.status === "completed") {
        const id = p.status === "success" ? "e0000001" : "e0000002"
        return new Promise((resolve) => held.push(() => resolve({ executions: [exec(id, p.status!, "2026-10-07T08:00:00Z")] })))
      }
      return Promise.resolve({ executions: [exec("e0000009", "failed", "2026-10-07T07:00:00Z")] })
    })
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getByText("e0000009...")).toBeInTheDocument())

    await user.click(screen.getByRole("combobox", { name: "Status" }))
    await user.click(screen.getByRole("option", { name: "Succeeded" }))
    await waitFor(() => expect(held).toHaveLength(2))
    await user.click(screen.getByRole("combobox", { name: "Status" }))
    await user.click(screen.getByRole("option", { name: "Failed" }))
    await waitFor(() => expect(screen.getByText("e0000009...")).toBeInTheDocument())

    held.forEach((release) => release())
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByText("e0000001...")).not.toBeInTheDocument()
    expect(screen.queryByText("e0000002...")).not.toBeInTheDocument()
    expect(screen.getByText("e0000009...")).toBeInTheDocument()
  })
})
