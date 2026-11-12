/**
 * The transform step on the pipeline pages, as in chat.
 *
 * Ask rsync.ai hands back a pipeline link (plan_pipeline → Create → "Open
 * pipeline"), so the user answers the run's table-selection pause on
 * /pipelines/<id>, not in chat. The pipeline pages resumed straight from the
 * table picker, so the AI transform suggestions never opened: neither for an
 * ordinary pause (picker → resume) nor for the one-time `review_transforms_only`
 * pause the orchestrator raises for tables that were already chosen
 * (transforms_review_gate.go), which exists only to show that step.
 *
 * Both panels must now go picker → transform step → resume, and open the step
 * straight away on a review-only pause. Nothing resumes before Apply or Skip.
 * The control: with no source connection known there is no step to show, and
 * the picker resumes the run as before.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/pipelines/p1",
}))

vi.mock("@/lib/hooks/usePipelineRuntime", () => ({
  usePipelineRuntime: () => ({ runtime: null, loading: false, error: null }),
}))

vi.mock("@/components/pipeline/MonitoringOverviewTab", () => ({ MonitoringOverviewTab: () => null }))
vi.mock("@/components/pipeline/TableStatisticsPanel", () => ({ TableStatisticsPanel: () => null }))
vi.mock("@/components/pipeline/ReasoningTimeline", () => ({ ReasoningTimeline: () => null }))

// The transform step itself (its rule editor, suggestion fetch and save) has its
// own tests; here only who opens it, with what, and what its two answers resume.
vi.mock("@/components/chat/SuggestionsReviewDialog", () => ({
  SuggestionsReviewDialog: (p: {
    isOpen: boolean
    selectedTables: string[]
    sourceConnectionId: string
    destinationType?: string
    onApplyContinue: (saved: { count: number }) => Promise<void>
    onClose: () => void | Promise<void>
  }) =>
    p.isOpen ? (
      <div data-testid="transform-step">
        <span data-testid="transform-step-tables">{p.selectedTables.join(",")}</span>
        <span data-testid="transform-step-source">{p.sourceConnectionId}</span>
        <span data-testid="transform-step-destination">{p.destinationType || ""}</span>
        <button onClick={() => void p.onApplyContinue({ count: 2 })}>Apply transforms</button>
        <button onClick={() => void p.onClose()}>Skip transforms</button>
      </div>
    ) : null,
}))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
  authFetchOrThrow: (...args: unknown[]) => authFetch(...args),
}))

vi.mock("@/lib/api/pipelines", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/pipelines")>()
  return {
    ...actual,
    resumePipelineTables: vi.fn(async () => ({ success: true })),
    resumePipelineNodeInput: vi.fn(async () => ({ success: true })),
  }
})

import { PipelineLiveStatePanel } from "@/components/pipeline/PipelineLiveStatePanel"
import { PipelineMonitoringPanel } from "@/components/pipeline/PipelineMonitoringPanel"
import { resumePipelineNodeInput, resumePipelineTables } from "@/lib/api/pipelines"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

function jsonOk(body: unknown) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) }
}

const TABLES = [
  { name: "invoices", schema: "public", row_count: 120, columns: 6 },
  { name: "payments", schema: "public", row_count: 80, columns: 5 },
]

// The /state of a pipeline parked on table selection, as pipeline_state.go serves it.
function waitingState(details: Record<string, unknown> = {}, tables = TABLES) {
  return {
    schema_version: 1,
    pipeline_id: "p1",
    execution_id: "e1",
    status: "waiting_for_user",
    current_stage: "executor",
    message: "Select which table(s)/resource(s) to sync (2 available).",
    progress: { percent: 40 },
    created_at: "2026-10-02T09:00:00Z",
    updated_at: "2026-10-02T09:01:00Z",
    blocking_reason: {
      type: "table_selection",
      description: "Select which table(s)/resource(s) to sync (2 available).",
      available_tables: tables,
      details: {
        request_type: "table_selection",
        action_needed: "table_selection",
        source_type: "postgresql",
        available_tables: tables,
        ...details,
      },
    },
  }
}

// The one-time pause for tables the plan already named (transformsReviewPause):
// the named tables are both the offered list and preselected_tables.
const NAMED = ["public.invoices"]
const reviewOnly = () =>
  waitingState(
    { review_transforms_only: true, preselected_tables: NAMED },
    NAMED.map((name) => ({ name, schema: "", row_count: 0, columns: 0 }))
  )

// pipeline_state.go never puts the source connection in the pause details; the
// pages read it from the pipeline, as the chat does.
const WITH_SOURCE = {
  source_connection_id: "conn-1",
  destination_connection: { connector_type: "bigquery" },
}

// A destination name is seeded so the picker's confirm button is enabled.
const PIPELINE = {
  id: "p1",
  name: "billing sync",
  sync_mode: "full",
  destination_config: { namespace: "analytics", namespace_kind: "schema", create_if_not_exists: true },
}

function routeWith(state: Record<string, unknown>, pipeline: Record<string, unknown> = {}) {
  authFetch.mockImplementation(async (url: string) => {
    const u = String(url)
    if (u.includes("/state")) return jsonOk(state)
    if (u.includes("/events")) return jsonOk({ events: [] })
    if (u.includes("/metadata")) return jsonOk({ tables: [{ name: "invoices", schema: "public", columns: [] }] })
    if (/\/pipelines\/p1(\?|$)/.test(u)) return jsonOk({ ...PIPELINE, ...pipeline })
    return jsonOk({})
  })
}

const resumeTables = resumePipelineTables as Mock
const resumeNode = resumePipelineNodeInput as Mock

beforeEach(() => {
  authFetch.mockReset()
  resumeTables.mockClear()
  resumeNode.mockClear()
})
afterEach(() => cleanup())

async function pickInvoicesAndSync() {
  const row = (await screen.findByText("public.invoices")).closest("label") as HTMLElement
  await userEvent.click(within(row).getByRole("checkbox"))
  await userEvent.click(await screen.findByRole("button", { name: "Sync 1 table" }))
}

describe("PipelineLiveStatePanel transform step", () => {
  it("a review-only pause opens the transform step on the named tables, with no picker and no resume", async () => {
    routeWith(reviewOnly(), WITH_SOURCE)
    render(<PipelineLiveStatePanel pipelineId="p1" />)

    expect(await screen.findByTestId("transform-step")).toBeInTheDocument()
    expect(screen.getByTestId("transform-step-tables")).toHaveTextContent("public.invoices")
    expect(screen.getByTestId("transform-step-source")).toHaveTextContent("conn-1")
    expect(screen.getByTestId("transform-step-destination")).toHaveTextContent("bigquery")
    expect(screen.queryByText("Select Tables to Sync")).toBeNull()
    expect(resumeTables).not.toHaveBeenCalled()
    expect(resumeNode).not.toHaveBeenCalled()
  })

  it("Apply resumes the run with the named tables and how many rules were saved", async () => {
    routeWith(reviewOnly(), WITH_SOURCE)
    render(<PipelineLiveStatePanel pipelineId="p1" />)
    await userEvent.click(await screen.findByRole("button", { name: "Apply transforms" }))

    await waitFor(() => expect(resumeTables).toHaveBeenCalledTimes(1))
    const [pid, body] = resumeTables.mock.calls[0]
    expect(pid).toBe("p1")
    expect(body).toMatchObject({ execution_id: "e1", selected_tables: NAMED, metadata: { transforms_saved: 2 } })
    // The rules live in transform_definitions; a transforms list here would override them.
    expect(body).not.toHaveProperty("transforms")
    await waitFor(() => expect(screen.queryByTestId("transform-step")).toBeNull())
  })

  it("an ordinary pause goes picker → transform step, and Skip resumes without transforms", async () => {
    routeWith(waitingState(), WITH_SOURCE)
    render(<PipelineLiveStatePanel pipelineId="p1" />)
    await pickInvoicesAndSync()

    expect(await screen.findByTestId("transform-step")).toBeInTheDocument()
    expect(screen.getByTestId("transform-step-tables")).toHaveTextContent("public.invoices")
    expect(resumeTables).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole("button", { name: "Skip transforms" }))
    await waitFor(() => expect(resumeTables).toHaveBeenCalledTimes(1))
    expect(resumeTables.mock.calls[0][1]).toMatchObject({
      execution_id: "e1",
      selected_tables: ["public.invoices"],
      metadata: { skipped_suggestions: true },
    })
  })

  it("a node-input pause answers the node, not the table signal", async () => {
    routeWith(waitingState({ node_id: "src-1" }), WITH_SOURCE)
    render(<PipelineLiveStatePanel pipelineId="p1" />)
    await pickInvoicesAndSync()
    await userEvent.click(await screen.findByRole("button", { name: "Apply transforms" }))

    await waitFor(() => expect(resumeNode).toHaveBeenCalledTimes(1))
    expect(resumeNode.mock.calls[0][1]).toMatchObject({
      execution_id: "e1",
      node_id: "src-1",
      config_patch: { selected_tables: ["public.invoices"], metadata: { transforms_saved: 2 } },
    })
    expect(resumeTables).not.toHaveBeenCalled()
  })

  it("control: with no source connection known the picker resumes at once, as before", async () => {
    routeWith(waitingState())
    render(<PipelineLiveStatePanel pipelineId="p1" />)
    await pickInvoicesAndSync()

    await waitFor(() => expect(resumeTables).toHaveBeenCalledTimes(1))
    expect(resumeTables.mock.calls[0][1]).toMatchObject({ execution_id: "e1", selected_tables: ["public.invoices"] })
    expect(screen.queryByTestId("transform-step")).toBeNull()
  })
})

// "Select tables" lives on the Table statistics page (variant="table_stats").
describe("PipelineMonitoringPanel transform step", () => {
  it("an ordinary pause goes picker → transform step, and Apply resumes", async () => {
    routeWith(waitingState(), WITH_SOURCE)
    render(<PipelineMonitoringPanel pipelineId="p1" variant="table_stats" />)
    await userEvent.click(await screen.findByRole("button", { name: "Select tables" }))
    await pickInvoicesAndSync()

    expect(await screen.findByTestId("transform-step")).toBeInTheDocument()
    expect(resumeTables).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole("button", { name: "Apply transforms" }))
    await waitFor(() => expect(resumeTables).toHaveBeenCalledTimes(1))
    expect(resumeTables.mock.calls[0][1]).toMatchObject({
      execution_id: "e1",
      selected_tables: ["public.invoices"],
      metadata: { transforms_saved: 2 },
    })
  })

  it("a review-only pause opens the transform step from its button, with no picker", async () => {
    routeWith(reviewOnly(), WITH_SOURCE)
    render(<PipelineMonitoringPanel pipelineId="p1" variant="table_stats" />)
    await userEvent.click(await screen.findByRole("button", { name: "Review transforms" }))

    expect(await screen.findByTestId("transform-step")).toBeInTheDocument()
    expect(screen.getByTestId("transform-step-tables")).toHaveTextContent("public.invoices")
    expect(screen.queryByText("Select Tables to Sync")).toBeNull()
    expect(resumeTables).not.toHaveBeenCalled()
  })

  it("control: with no source connection known the picker resumes at once, as before", async () => {
    routeWith(waitingState())
    render(<PipelineMonitoringPanel pipelineId="p1" variant="table_stats" />)
    await userEvent.click(await screen.findByRole("button", { name: "Select tables" }))
    await pickInvoicesAndSync()

    await waitFor(() => expect(resumeTables).toHaveBeenCalledTimes(1))
    expect(screen.queryByTestId("transform-step")).toBeNull()
  })
})
