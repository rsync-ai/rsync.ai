/**
 * The Pipelines list for launch: each end of a pipeline is its connection's name
 * beside its connector's logo, the type sits under the name in plain words (no
 * Type column), "passed" reads "Succeeded" as on Runs and Home, loading is
 * skeleton rows, the two empty cases are EmptyStates, and the two bare date
 * inputs are one "Any time" button whose range is sent as local day boundaries.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: "admin", can: () => true }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

import { authFetch } from "@/lib/api/auth-fetch"
import { PipelinesPageClient } from "@/app/(dashboard)/pipelines/PipelinesPageClient"
import { PipelinesTable, type PipelineListItem } from "@/components/pipeline/PipelinesTable"
import { createdRangeLabel, createdRangeParams } from "@/components/pipeline/CreatedRangeFilter"

const mockAuthFetch = vi.mocked(authFetch)

function row(over: Partial<PipelineListItem> = {}): PipelineListItem {
  return {
    id: "p1",
    name: "orders_sync",
    description: "Nightly orders copy",
    pipeline_status: "active",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    derived_status: "passed",
    sync_mode: "batch",
    source_connection: { name: "orders-db", connector_type: "postgresql" },
    destination_connection: { name: "warehouse", connector_type: "bigquery" },
    ...over,
  }
}

function json(body: unknown): Response {
  return { ok: true, status: 200, json: async () => body } as unknown as Response
}

function listUrls(): URL[] {
  return mockAuthFetch.mock.calls
    .map(([u]) => String(u))
    .filter((u) => u.includes("/api/v1/pipelines?"))
    .map((u) => new URL(u, "http://x"))
}

describe("PipelinesTable row", () => {
  it("shows both ends as logo + connection name, and no Type column", () => {
    render(<PipelinesTable pipelines={[row()]} />)
    const table = screen.getByRole("table")
    expect(within(table).queryByRole("columnheader", { name: /^type$/i })).toBeNull()
    expect(within(table).getByRole("columnheader", { name: /^pipeline$/i })).toBeInTheDocument()
    expect(within(table).getByText("orders-db")).toBeInTheDocument()
    expect(within(table).getByText("warehouse")).toBeInTheDocument()
    expect(within(table).getByAltText("postgresql logo")).toBeInTheDocument()
    expect(within(table).getByAltText("bigquery logo")).toBeInTheDocument()
  })

  it("puts the type and then the description under the name, in plain type", () => {
    render(<PipelinesTable pipelines={[row()]} />)
    const line = screen.getByTestId("pipeline-type-line")
    expect(line).toHaveTextContent("Batch (manual) · Nightly orders copy")
    expect(line.className).not.toContain("font-mono")
  })

  it("names the type alone when there is no description", () => {
    render(<PipelinesTable pipelines={[row({ description: undefined, sync_mode: "cdc", cdc_mode: "streaming_only" })]} />)
    expect(screen.getByTestId("pipeline-type-line")).toHaveTextContent(/^CDC \(changes only\)$/)
  })

  it('reads "passed" as Succeeded, the word Runs and Home use', () => {
    render(<PipelinesTable pipelines={[row({ derived_status: "passed" })]} />)
    expect(screen.getByText(/^succeeded$/i)).toBeInTheDocument()
    expect(screen.queryByText(/^completed$/i)).toBeNull()
  })
})

describe("created range params", () => {
  const now = new Date(2026, 9, 7, 15, 30) // 7 Oct 2026, 15:30 local

  it("sends nothing for Any time", () => {
    expect(createdRangeParams({ kind: "any" }, now)).toEqual({})
    expect(createdRangeLabel({ kind: "any" })).toBe("Any time")
  })

  it("starts a preset at local midnight N days back", () => {
    const p = createdRangeParams({ kind: "last", days: 7 }, now)
    expect(p.to).toBeUndefined()
    expect(new Date(p.from!).getTime()).toBe(new Date(2026, 8, 30).getTime())
    expect(createdRangeLabel({ kind: "last", days: 7 })).toBe("Last 7 days")
  })

  it("sends a custom range as whole local days, the last day included", () => {
    const p = createdRangeParams({ kind: "custom", from: "2026-09-01", to: "2026-09-30" }, now)
    expect(new Date(p.from!).getTime()).toBe(new Date(2026, 8, 1).getTime())
    expect(new Date(p.to!).getTime()).toBe(new Date(2026, 8, 30, 23, 59, 59, 999).getTime())
  })

  it("leaves an open side of a custom range unsent", () => {
    expect(createdRangeParams({ kind: "custom", from: "2026-09-01", to: "" }, now).to).toBeUndefined()
    expect(createdRangeLabel({ kind: "custom", from: "", to: "" })).toBe("Any time")
  })
})

describe("/pipelines page states", () => {
  let pipelines: PipelineListItem[] = []

  beforeEach(() => {
    pipelines = []
    mockAuthFetch.mockReset()
    mockAuthFetch.mockImplementation(async () => json({ pipelines, total: pipelines.length }))
  })

  it("shows skeleton rows while the first page loads", async () => {
    let release: (r: Response) => void = () => {}
    mockAuthFetch.mockImplementationOnce(() => new Promise<Response>((r) => (release = r)))
    render(<PipelinesPageClient />)
    const statuses = screen.getAllByRole("status").map((s) => s.textContent)
    expect(statuses).toEqual(expect.arrayContaining([expect.stringContaining("Loading pipelines…")]))
    release(json({ pipelines: [], total: 0 }))
    expect(await screen.findByText("No pipelines yet")).toBeInTheDocument()
  })

  it("offers Create with AI when there is no pipeline at all", async () => {
    render(<PipelinesPageClient />)
    expect(await screen.findByText("No pipelines yet")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /create with ai/i })).toBeInTheDocument()
    expect(screen.queryByText(/no pipelines match/i)).toBeNull()
  })

  it("says the filters matched nothing, and Clear filters drops them", async () => {
    const user = userEvent.setup()
    render(<PipelinesPageClient />)
    await screen.findByText("No pipelines yet")

    await user.type(screen.getByPlaceholderText(/search pipelines/i), "zzz")
    expect(await screen.findByText("No pipelines match your filters")).toBeInTheDocument()
    expect(screen.queryByText("No pipelines yet")).toBeNull()
    await waitFor(() => expect(listUrls().at(-1)!.searchParams.get("q")).toBe("zzz"))

    await user.click(screen.getByRole("button", { name: /^clear filters$/i }))
    expect(await screen.findByText("No pipelines yet")).toBeInTheDocument()
    await waitFor(() => expect(listUrls().at(-1)!.searchParams.get("q")).toBeNull())
  })

  it("one Any time button sets created_from from a preset", async () => {
    pipelines = [row()]
    const user = userEvent.setup()
    render(<PipelinesPageClient />)
    await screen.findByRole("table")
    expect(listUrls().at(-1)!.searchParams.get("created_from")).toBeNull()

    await user.click(screen.getByRole("button", { name: "Created: Any time" }))
    await user.click(await screen.findByRole("button", { name: "Last 30 days" }))

    expect(screen.getByRole("button", { name: "Created: Last 30 days" })).toBeInTheDocument()
    await waitFor(() => expect(listUrls().at(-1)!.searchParams.get("created_from")).not.toBeNull())
    const from = new Date(listUrls().at(-1)!.searchParams.get("created_from")!)
    const days = (Date.now() - from.getTime()) / 86_400_000
    expect(days).toBeGreaterThanOrEqual(30)
    expect(days).toBeLessThan(31)
    expect(listUrls().at(-1)!.searchParams.get("created_to")).toBeNull()
  })
})
