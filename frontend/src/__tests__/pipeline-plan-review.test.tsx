import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { AssistantThread, planIds } from "@/components/assistant/AssistantThread"
import { PipelinePlanReview } from "@/components/pipeline-plans/PipelinePlanReview"
import { authFetch } from "@/lib/api/auth-fetch"
import type { AssistantAnswer } from "@/lib/api/assistant"
import { API_ENDPOINTS } from "@/lib/config/api"

// A pipeline Ask rsync.ai or an agent proposed (plan_pipeline). The server owns
// every rule — who may read or apply a plan, the once-only claim, the 24 h
// expiry, which tables a pick may name — so what is pinned here is what only
// the card can get wrong:
//   - nothing is sent until Create is clicked, and then one POST to apply;
//   - a name in two schemas blocks Create until the person picks one, and the
//     pick is sent with the plan's own tables (it replaces the list);
//   - an applied or expired plan offers no Create, and a refused Create shows
//     the server's sentence and what the plan is now;
//   - Ask rsync.ai shows a card for each plan an answer stored, never for a
//     refused call.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as unknown as Mock
const PLAN = "ffffffff-0000-4000-8000-000000000001"
const PIPE = "55555555-5555-4555-8555-555555555555"
const future = new Date(Date.now() + 86_400_000).toISOString()

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function plan(overrides: Record<string, unknown> = {}) {
  return {
    id: PLAN,
    status: "proposed",
    proposed_by: "assistant",
    source: { id: "s1", name: "Shop DB", connector_type: "postgresql" },
    destination: { id: "d1", name: "Warehouse", connector_type: "bigquery" },
    sync_mode: "batch",
    tables: ["public.orders", "public.refunds"],
    tables_checked: true,
    schedule: { type: "cron", cron: "0 * * * *", timezone: "UTC", description: "every hour (UTC)" },
    notes: ["Not found in the source, so left out: `ghosts`."],
    url: `https://app.example/pipeline-plans/${PLAN}`,
    created_at: new Date().toISOString(),
    expires_at: future,
    ...overrides,
  }
}

// Routes authFetch: GET the plan (the next of `reads`, then the last again),
// POST apply (the next of `applies`). Returns the apply bodies sent.
function route(opts: { reads?: Response[]; applies?: Response[] } = {}) {
  const reads = [...(opts.reads ?? [json(200, plan())])]
  const applies = [...(opts.applies ?? [json(201, { pipeline_id: PIPE, status: "scheduled", schedule: "every hour (UTC)" })])]
  const sent: unknown[] = []
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === API_ENDPOINTS.PIPELINE_PLANS.GET(PLAN) && (init?.method ?? "GET") === "GET") {
      return Promise.resolve(reads.length > 1 ? reads.shift()! : reads[0])
    }
    if (url === API_ENDPOINTS.PIPELINE_PLANS.APPLY(PLAN) && init?.method === "POST") {
      sent.push(JSON.parse(String(init.body)))
      return Promise.resolve(applies.shift() ?? json(500, { error: "unexpected" }))
    }
    return Promise.resolve(json(404, { error: "not_found" }))
  })
  return sent
}

const posts = () => mockFetch.mock.calls.filter(([, i]) => i?.method === "POST")

beforeAll(() => {
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

beforeEach(() => mockFetch.mockReset())

describe("PipelinePlanReview", () => {
  it("shows the plan and sends nothing until Create; then one POST with the plan's own tables", async () => {
    const sent = route()
    render(<PipelinePlanReview planId={PLAN} />)
    const card = await screen.findByRole("region", { name: "Proposed pipeline" })
    expect(within(card).getByText("Shop DB")).toBeInTheDocument()
    expect(within(card).getByText("Warehouse")).toBeInTheDocument()
    expect(within(card).getByText("every hour (UTC)")).toBeInTheDocument()
    expect(within(within(card).getByRole("list", { name: "Tables" })).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "public.orders",
      "public.refunds",
    ])
    expect(within(card).getByText(/left out/)).toBeInTheDocument()
    expect(within(card).getByText("Waiting for you")).toBeInTheDocument()
    expect(posts()).toHaveLength(0)

    fireEvent.click(within(card).getByRole("button", { name: /create pipeline/i }))
    await within(card).findByText(/Created\. It runs every hour \(UTC\)\./)
    // No choice open: the plan's own tables, so no list is sent.
    expect(sent).toEqual([{}])
    expect(within(card).getByRole("link", { name: "Open pipeline" })).toHaveAttribute("href", `/pipelines/${PIPE}`)
    expect(within(card).queryByRole("button", { name: /create pipeline/i })).toBeNull()
  })

  it("shows the names a note quotes in backticks as code, on the plan and after Create", async () => {
    route({
      reads: [json(200, plan({ notes: ["Not found in the source, so left out: `ghosts`, `public.old`.", "An odd ` stays."] }))],
      applies: [json(201, { pipeline_id: PIPE, status: "running", note: "📋 Pre-selected only the tables you named: `public.orders`." })],
    })
    render(<PipelinePlanReview planId={PLAN} />)
    const card = await screen.findByRole("region", { name: "Proposed pipeline" })
    const notes = within(card).getAllByRole("listitem").filter((li) => li.closest("ul")?.getAttribute("aria-label") === "Notes")
    expect(notes.map((li) => li.textContent)).toEqual(["Not found in the source, so left out: ghosts, public.old.", "An odd ` stays."])
    expect(Array.from(notes[0].querySelectorAll("code")).map((c) => c.textContent)).toEqual(["ghosts", "public.old"])
    expect(notes[1].querySelector("code")).toBeNull()

    fireEvent.click(within(card).getByRole("button", { name: /create pipeline/i }))
    const note = await within(card).findByText(/Pre-selected only the tables you named/)
    expect(note.textContent).toBe("📋 Pre-selected only the tables you named: public.orders.")
    expect(note.querySelector("code")?.textContent).toBe("public.orders")
  })

  it("blocks Create until a schema is picked for a name in two, then sends the plan's tables and the pick", async () => {
    const sent = route({
      reads: [json(200, plan({ tables: ["public.orders"], table_choices: [{ name: "customers", options: ["crm.customers", "public.customers"] }] }))],
      applies: [json(201, { pipeline_id: PIPE, status: "running" })],
    })
    render(<PipelinePlanReview planId={PLAN} />)
    const create = await screen.findByRole("button", { name: /create pipeline/i })
    expect(create).toBeDisabled()
    expect(screen.getByText(/pick a schema for each table/i)).toBeInTheDocument()

    fireEvent.click(within(screen.getByRole("radiogroup", { name: "Schema for customers" })).getByRole("radio", { name: "crm.customers" }))
    await waitFor(() => expect(create).toBeEnabled())
    fireEvent.click(create)
    await screen.findByText(/its first run started/i)
    expect(sent).toEqual([{ tables: ["public.orders", "crm.customers"] }])
  })

  it("a refused Create shows the server's sentence and what the plan is now", async () => {
    const sent = route({
      reads: [json(200, plan()), json(200, plan({ status: "applied", applied_pipeline_id: PIPE }))],
      applies: [
        json(409, { error: "This plan was already used to create a pipeline.", code: "already_applied", applied_pipeline_id: PIPE }),
      ],
    })
    render(<PipelinePlanReview planId={PLAN} />)
    fireEvent.click(await screen.findByRole("button", { name: /create pipeline/i }))
    expect(await screen.findByRole("alert")).toHaveTextContent("This plan was already used to create a pipeline.")
    expect(await screen.findByRole("link", { name: "Open pipeline" })).toHaveAttribute("href", `/pipelines/${PIPE}`)
    expect(screen.queryByRole("button", { name: /create pipeline/i })).toBeNull()
    expect(sent).toHaveLength(1)
  })

  it("an expired plan offers no Create", async () => {
    route({ reads: [json(200, plan({ status: "expired" }))] })
    render(<PipelinePlanReview planId={PLAN} />)
    expect(await screen.findByText(/this plan expired/i)).toBeInTheDocument()
    expect(screen.getByText("Expired")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /create pipeline/i })).toBeNull()
  })

  it("someone else's plan (404) says so, with no Retry", async () => {
    route({ reads: [json(404, { error: "pipeline plan not found" })] })
    render(<PipelinePlanReview planId={PLAN} />)
    expect(await screen.findByRole("alert")).toHaveTextContent(/only for the person it was proposed for/i)
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull()
  })

  it("the card lists a few tables and links to the page for the rest", async () => {
    const tables = Array.from({ length: 12 }, (_, i) => `public.t${i}`)
    route({ reads: [json(200, plan({ tables }))] })
    render(<PipelinePlanReview planId={PLAN} compact />)
    const list = await screen.findByRole("list", { name: "Tables" })
    expect(within(list).getAllByRole("listitem")).toHaveLength(8)
    expect(screen.getByRole("link", { name: "and 4 more" })).toHaveAttribute("href", `/pipeline-plans/${PLAN}`)
  })
})

describe("Ask rsync.ai's plan card", () => {
  const base: AssistantAnswer = {
    conversation_id: "conv-1",
    answer: "I proposed a pipeline from Shop DB to Warehouse.",
    model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
    looked_at: [],
    cost_cents: 1,
  }

  it("takes each stored plan once, and none from a refused call or another tool", () => {
    expect(
      planIds({
        ...base,
        looked_at: [
          { tool: "list_connections", arguments: {}, ok: true, resource_id: "x" },
          { tool: "plan_pipeline", arguments: {}, ok: false, resource_id: "refused" },
          { tool: "plan_pipeline", arguments: {}, ok: true, resource_id: PLAN },
          { tool: "plan_pipeline", arguments: {}, ok: true },
        ],
      }),
    ).toEqual([PLAN])
  })

  it("shows the card under the answer", async () => {
    route()
    const answer = { ...base, looked_at: [{ tool: "plan_pipeline", arguments: {}, ok: true, resource_id: PLAN }] }
    render(
      <AssistantThread
        chat={{ id: "c1", workspaceId: null, title: "copy", items: [{ kind: "answer", answer }], updatedAt: Date.now() }}
        wide={false}
        onRetry={() => {}}
      />,
    )
    const card = await screen.findByRole("region", { name: "Proposed pipeline" })
    expect(within(card).getByRole("button", { name: /create pipeline/i })).toBeInTheDocument()
    expect(screen.getByText(/Looked at a pipeline proposal/)).toBeInTheDocument()
  })
})
