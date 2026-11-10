import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor, fireEvent } from "@testing-library/react"
import { toast } from "sonner"
import type { Mock } from "vitest"

import { ExecutionDetailClient } from "@/components/executions/ExecutionDetailClient"

// The run endpoint refuses a run while another one is copying data:
//   409 {"error": "run_in_progress", "message": "This pipeline already has a run in progress. …"}
// The pipeline page's Run button shows that sentence. The execution page's Re-run
// buttons logged it to the console and stopped spinning, so a refused re-run looked
// like a click that did nothing.

const push = vi.fn()
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, refresh: vi.fn() }),
}))
vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: (...args: unknown[]) => authFetch(...args) }))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  vi.clearAllMocks()
})

const errorToast = toast.error as unknown as Mock
const inProgress =
  "This pipeline already has a run in progress. Wait for it to finish or stop it, then run again."

// The page passes the status through normalizeExecutionStatus, which reads a
// completed run as "success".
function renderFinishedRun() {
  render(
    <ExecutionDetailClient
      execution={{ id: "exec-1", status: "success", pipelineId: "pipe-1" }}
    />,
  )
}

describe("Re-run on the execution page", () => {
  it("says why the server refused it", async () => {
    authFetch.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "run_in_progress", message: inProgress, execution_id: "exec-2" }),
    })

    renderFinishedRun()
    fireEvent.click(screen.getByRole("button", { name: /re-run \(resume\)/i }))

    await waitFor(() => expect(errorToast).toHaveBeenCalled())
    expect(errorToast).toHaveBeenCalledWith("Could not start pipeline", { description: inProgress })
    expect(push).not.toHaveBeenCalled()
  })

  // The control: an accepted re-run opens the new execution and raises nothing.
  it("opens the new run when the server accepts it", async () => {
    authFetch.mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ execution_id: "exec-3" }),
    })

    renderFinishedRun()
    fireEvent.click(screen.getByRole("button", { name: /re-run \(reload\)/i }))

    await waitFor(() => expect(push).toHaveBeenCalledWith("/executions/exec-3"))
    expect(errorToast).not.toHaveBeenCalled()
  })
})
