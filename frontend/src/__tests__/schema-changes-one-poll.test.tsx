/**
 * The Steps tab mounts the header's drift badge AND the schema panel for the
 * same pipeline. Each used to run its own 30 s poll on /schema-changes, so the
 * tab read it twice, and approving a change in the panel left the badge counting
 * it until the badge's own next tick. Both now read through one shared poller.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))

import { SchemaEvolutionPanel } from "@/components/pipeline/SchemaEvolutionPanel"
import { SchemaDriftBadge } from "@/components/pipeline/SchemaDriftBadge"

const mockFetch = vi.fn()
vi.stubGlobal("fetch", mockFetch)

function okJson(body: unknown) {
  return {
    ok: true,
    status: 200,
    statusText: "OK",
    text: async () => JSON.stringify(body),
    json: async () => body,
  } as unknown as Response
}

const PENDING = {
  id: "sc-1",
  pipeline_id: "p1",
  change_type: "add_column",
  table_name: "public.orders",
  ddl: "ALTER TABLE public.orders ADD COLUMN note TEXT",
  reasoning: "source added a column",
  risks: "[]",
  user_message: "New column detected",
  status: "pending" as const,
  reviewed_by: null,
  reviewed_at: null,
  applied_at: null,
}

/** GETs of the list route, not the approve/reject POSTs under it. */
function listReads(): number {
  return mockFetch.mock.calls.filter(([url, init]) => {
    const method = (init as RequestInit | undefined)?.method ?? "GET"
    return method === "GET" && String(url).endsWith("/pipelines/p1/schema-changes")
  }).length
}

function StepsTab() {
  return (
    <>
      <SchemaDriftBadge pipelineId="p1" />
      <SchemaEvolutionPanel pipelineId="p1" />
    </>
  )
}

describe("schema changes — one read for the badge and the panel", () => {
  beforeEach(() => vi.clearAllMocks())

  it("the badge and the panel mounted together read the list once", async () => {
    mockFetch.mockResolvedValue(okJson({ schema_changes: [PENDING] }))

    render(<StepsTab />)

    expect(await screen.findByText("public.orders")).toBeInTheDocument()
    expect(await screen.findByText("1 schema change")).toBeInTheDocument()
    expect(listReads()).toBe(1)
  })

  it("approving in the panel clears the badge at once, without waiting for its poll", async () => {
    let approved = false
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      if (init?.method === "POST") {
        approved = true
        return okJson({})
      }
      return okJson({ schema_changes: approved ? [{ ...PENDING, status: "applied" }] : [PENDING] })
    })

    render(<StepsTab />)
    expect(await screen.findByText("1 schema change")).toBeInTheDocument()

    await userEvent.click(await screen.findByRole("button", { name: /apply migration/i }))

    // Well inside the 30 s poll: only the panel's re-read can explain it.
    await waitFor(() => expect(screen.queryByText("1 schema change")).not.toBeInTheDocument())
    expect(listReads()).toBe(2)
  })
})
