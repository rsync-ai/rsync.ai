/**
 * Transforms are added and changed in the pipeline's own Transforms tab. They
 * used to be authored on a separate Transform Builder page with a pipeline
 * picker, while this tab only listed them — and on a pipeline with no rules it
 * said rsync.ai "didn't detect any columns that need shaping", which read as a
 * check that ran and passed on a pipeline where no transform step was offered.
 *
 * The editor itself is covered by transform-plan-editor.test.tsx; it is stubbed
 * here so a failure in this file is about the tab: when the editor opens, what
 * the tab says around it, and that a save is re-read.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

let mockRole = "member"

const authGet = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authGet: (...args: unknown[]) => authGet(...args),
  authFetch: vi.fn(),
}))

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }))

vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: mockRole,
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => false,
    meets: (min: string) => realMeetsRole(mockRole, min as never),
  }),
}))

vi.mock("@/components/transforms/TransformMonitoringPanel", () => ({
  TransformMonitoringPanel: () => null,
}))
vi.mock("@/components/transforms/TransformExecutionLogsPanel", () => ({
  TransformExecutionLogsPanel: () => null,
}))
vi.mock("@/components/transforms/TransformPlanEditor", () => ({
  TransformPlanEditor: ({ pipelineId, onClose }: { pipelineId: string; onClose: (saved: boolean) => void }) => (
    <div data-testid="transform-plan-editor" data-pipeline={pipelineId}>
      <button onClick={() => onClose(true)}>stub save</button>
      <button onClick={() => onClose(false)}>stub cancel</button>
    </div>
  ),
}))

import { meetsRole as realMeetsRole } from "@/lib/workspace/roles"
import { PipelineTransformsTab } from "@/components/pipeline/PipelineTransformsTab"

const EMPTY = { producer_transforms: [], consumer_transforms: [] }
const MASK = { operation: "mask_pii", column: "email", mask_type: "hash" }
const ONE_RULE = {
  producer_transforms: [{ id: "b", transform_type: "producer", transform_order: 0, transform_config: MASK, enabled: true }],
  consumer_transforms: [{ id: "c", transform_type: "consumer", transform_order: 0, transform_config: MASK, enabled: true }],
}

function serve(plan: unknown) {
  authGet.mockImplementation(async (url: string) => {
    if (String(url).includes("/executions")) return { executions: [] }
    return plan
  })
}

const planReads = () => authGet.mock.calls.filter(([url]) => String(url).includes("/transforms/pipeline/")).length

beforeEach(() => {
  mockRole = "member"
  authGet.mockReset()
})
afterEach(() => cleanup())

describe("PipelineTransformsTab — adding and editing in the tab", () => {
  it("says plainly that nothing is stored, and offers to add transforms", async () => {
    serve(EMPTY)
    render(<PipelineTransformsTab pipelineId="p1" />)

    expect(
      await screen.findByText("No transforms on this pipeline. Rows are written to the destination as the source has them.")
    ).toBeInTheDocument()
    expect(screen.queryByText(/didn.t detect any columns/i)).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Add transforms" })).toBeInTheDocument()
  })

  it("opens the editor for this pipeline in place of the list", async () => {
    serve(EMPTY)
    render(<PipelineTransformsTab pipelineId="p1" />)

    await userEvent.setup().click(await screen.findByRole("button", { name: "Add transforms" }))

    expect(screen.getByTestId("transform-plan-editor")).toHaveAttribute("data-pipeline", "p1")
    expect(screen.queryByText(/No transforms on this pipeline/)).not.toBeInTheDocument()
  })

  it("re-reads the stored plan after a save, so the list shows what the server kept", async () => {
    serve(EMPTY)
    render(<PipelineTransformsTab pipelineId="p1" />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole("button", { name: "Add transforms" }))
    expect(planReads()).toBe(1)

    serve(ONE_RULE)
    await user.click(screen.getByRole("button", { name: "stub save" }))

    await waitFor(() => expect(planReads()).toBe(2))
    expect(await screen.findByText("1 transform active")).toBeInTheDocument()
    expect(screen.queryByTestId("transform-plan-editor")).not.toBeInTheDocument()
  })

  it("does not re-read when the editor is left without a save", async () => {
    // Non-zero control for the test above.
    serve(EMPTY)
    render(<PipelineTransformsTab pipelineId="p1" />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole("button", { name: "Add transforms" }))

    await user.click(screen.getByRole("button", { name: "stub cancel" }))

    expect(await screen.findByRole("button", { name: "Add transforms" })).toBeInTheDocument()
    expect(planReads()).toBe(1)
  })

  it("offers to edit a pipeline that has rules, and no longer calls them auto-applied", async () => {
    serve(ONE_RULE)
    render(<PipelineTransformsTab pipelineId="p1" />)

    expect(await screen.findByText("1 transform active")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Add or edit transforms" })).toBeInTheDocument()
    // Rules are picked in the transform step or added in the editor; nothing
    // applies them on its own.
    expect(screen.queryByText(/auto-applied/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/^Auto$/)).not.toBeInTheDocument()
  })

  it("gives a viewer no way into the editor", async () => {
    mockRole = "viewer"
    serve(ONE_RULE)
    const { unmount } = render(<PipelineTransformsTab pipelineId="p1" />)
    expect(await screen.findByText("1 transform active")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Add or edit transforms" })).not.toBeInTheDocument()
    unmount()

    serve(EMPTY)
    render(<PipelineTransformsTab pipelineId="p1" />)
    expect(await screen.findByText(/No transforms on this pipeline/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Add transforms" })).not.toBeInTheDocument()
  })
})
