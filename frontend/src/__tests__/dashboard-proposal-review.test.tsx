import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"

import { dashboardProposalIds } from "@/components/assistant/AssistantThread"
import { DashboardProposalReview } from "@/components/dashboards/DashboardProposalReview"
import { authFetch } from "@/lib/api/auth-fetch"
import type { AssistantAnswer } from "@/lib/api/assistant"
import type { DashboardProposal } from "@/lib/api/dashboards"
import { API_ENDPOINTS } from "@/lib/config/api"

// A dashboard Ask rsync.ai drafted (plan_dashboard, dashboards-design.md § 7.3).
// The server owns every rule: who may save, the settings' checks, the run-as
// user. What is pinned here is what only the card can get wrong:
//   - loading the card saves nothing;
//   - Save is one POST with who sees it, then opens the dashboard;
//   - a person who cannot manage gets no Save;
//   - a saved, expired or missing draft says so and offers no Save;
//   - Ask rsync.ai shows a card for each draft an answer stored, never for a
//     refused call.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
const push = vi.fn()
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const DP = "dddddddd-0000-4000-8000-0000000000d1"
const DASH = "dddddddd-0000-4000-8000-0000000000a1"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function proposal(overrides: Partial<DashboardProposal> = {}): DashboardProposal {
  return {
    id: DP,
    name: "Orders",
    description: "Orders by day and region",
    charts: [
      {
        name: "Orders by day",
        source_id: "s1",
        source_name: "Daily orders",
        source_kind: "saved_query",
        type: "line",
        settings: { version: 2, type: "line", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } },
      },
      {
        name: "Orders by region",
        source_id: "s1",
        source_name: "Daily orders",
        source_kind: "saved_query",
        type: "hbar",
        settings: { version: 2, type: "hbar", roles: { x: [{ column: "region" }], measure: [{ aggregate: "count" }] } },
      },
    ],
    filters: [{ id: "region", kind: "values", label: "Region", column: "region" }],
    tiles: [
      { chart: 0, x: 0, y: 0, w: 6, h: 7 },
      { chart: 1, x: 6, y: 0, w: 6, h: 7 },
    ],
    status: "proposed",
    expired: false,
    proposed_by: "assistant",
    created_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    url: `https://app.example/dashboard-proposals/${DP}`,
    ...overrides,
  }
}

function route(opts: { get?: Response; save?: Response; canManage?: boolean } = {}) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.DASHBOARD_PROPOSALS.GET(DP) && method === "GET") {
      return Promise.resolve(opts.get ?? json(200, { proposal: proposal(), can_manage: opts.canManage ?? true }))
    }
    if (url === API_ENDPOINTS.DASHBOARD_PROPOSALS.SAVE(DP) && method === "POST") {
      return Promise.resolve(opts.save ?? json(201, { dashboard_id: DASH, chart_ids: ["c1", "c2"] }))
    }
    return Promise.resolve(json(500, { error: `unexpected ${method} ${url}` }))
  })
}

const posts = () => mockFetch.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === "POST")

beforeEach(() => {
  mockFetch.mockReset()
  push.mockReset()
})

describe("DashboardProposalReview", () => {
  it("lists the draft's charts and filters and saves nothing on load", async () => {
    route()
    render(<DashboardProposalReview proposalId={DP} />)
    expect(screen.getByRole("status")).toHaveTextContent("Loading the dashboard draft…")
    const card = await screen.findByRole("region", { name: "Dashboard draft: Orders" })
    expect(card).toHaveTextContent("Orders by day · Line of Daily orders")
    expect(card).toHaveTextContent("Orders by region · Horizontal bar of Daily orders")
    expect(card).toHaveTextContent("Filters: Region (region)")
    expect(card).toHaveTextContent("Drafted by Ask rsync.ai")
    expect(screen.getByRole("link", { name: "Preview" })).toHaveAttribute("href", `/dashboard-proposals/${DP}`)
    expect(posts()).toHaveLength(0)
  })

  it("saves with who sees it, then opens the dashboard", async () => {
    route()
    render(<DashboardProposalReview proposalId={DP} />)
    fireEvent.change(await screen.findByLabelText("Who sees it"), { target: { value: "workspace" } })
    fireEvent.click(screen.getByRole("button", { name: /Save dashboard/ }))
    await waitFor(() => expect(push).toHaveBeenCalledWith(`/dashboards/${DASH}`))
    const sent = posts()
    expect(sent).toHaveLength(1)
    expect(sent[0][0]).toBe(API_ENDPOINTS.DASHBOARD_PROPOSALS.SAVE(DP))
    expect(JSON.parse((sent[0][1] as RequestInit).body as string)).toEqual({ visibility: "workspace" })
  })

  it("keeps a draft private unless the person picks the workspace", async () => {
    route()
    render(<DashboardProposalReview proposalId={DP} />)
    fireEvent.click(await screen.findByRole("button", { name: /Save dashboard/ }))
    await waitFor(() => expect(posts()).toHaveLength(1))
    expect(JSON.parse((posts()[0][1] as RequestInit).body as string)).toEqual({ visibility: "private" })
  })

  it("gives a person who cannot manage no Save", async () => {
    route({ canManage: false })
    render(<DashboardProposalReview proposalId={DP} />)
    expect(await screen.findByText("Workspace owners and admins save dashboards.")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
    expect(screen.queryByLabelText("Who sees it")).toBeNull()
  })

  it("says a draft from another workspace was not found, with Retry", async () => {
    route({ get: json(404, { error: "Not found" }) })
    render(<DashboardProposalReview proposalId={DP} />)
    expect(await screen.findByRole("alert")).toHaveTextContent("This dashboard draft was not found in this workspace.")
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
  })

  it("says an expired draft expired and offers no Save", async () => {
    route({ get: json(200, { proposal: proposal({ expired: true }), can_manage: true }) })
    render(<DashboardProposalReview proposalId={DP} />)
    expect(await screen.findByText(/This draft expired/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
  })

  it("links a saved draft to its dashboard and offers no Save", async () => {
    route({ get: json(200, { proposal: proposal({ status: "saved", saved_dashboard_id: DASH }), can_manage: true }) })
    render(<DashboardProposalReview proposalId={DP} />)
    expect(await screen.findByRole("link", { name: "Open the dashboard" })).toHaveAttribute("href", `/dashboards/${DASH}`)
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
  })

  it("links the dashboard another person saved first, when Save finds it already saved", async () => {
    route({
      save: json(409, { error: "This draft is already saved.", code: "already_saved", dashboard_id: DASH }),
    })
    render(<DashboardProposalReview proposalId={DP} />)
    fireEvent.click(await screen.findByRole("button", { name: /Save dashboard/ }))
    expect(await screen.findByRole("link", { name: "Open the dashboard" })).toHaveAttribute("href", `/dashboards/${DASH}`)
    expect(screen.getByRole("alert")).toHaveTextContent("This draft is already saved.")
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
    expect(push).not.toHaveBeenCalled()
  })

  it("says the draft expired when Save finds it expired", async () => {
    route({ save: json(410, { error: "This draft expired. Ask rsync.ai to draft it again.", code: "expired" }) })
    render(<DashboardProposalReview proposalId={DP} />)
    fireEvent.click(await screen.findByRole("button", { name: /Save dashboard/ }))
    expect(await screen.findByText(/an unsaved draft is kept for 24 hours/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Save dashboard/ })).toBeNull()
    expect(push).not.toHaveBeenCalled()
  })

  it("shows a refused save's reason and keeps Save", async () => {
    route({ save: json(422, { error: "Chart 2: the column region is not in the source's result.", rule: 2 }) })
    render(<DashboardProposalReview proposalId={DP} />)
    fireEvent.click(await screen.findByRole("button", { name: /Save dashboard/ }))
    expect(await screen.findByRole("alert")).toHaveTextContent("Chart 2: the column region is not in the source's result.")
    expect(screen.getByRole("button", { name: /Save dashboard/ })).toBeEnabled()
    expect(push).not.toHaveBeenCalled()
  })
})

describe("Ask rsync.ai's dashboard cards", () => {
  it("shows a card for each stored draft, never for a refused call", () => {
    const answer = {
      answer: "Drafted.",
      looked_at: [
        { tool: "plan_dashboard", ok: true, resource_id: DP },
        { tool: "plan_dashboard", ok: false, resource_id: "refused" },
        { tool: "plan_dashboard", ok: true },
        { tool: "plan_workflow", ok: true, resource_id: "a-workflow" },
        { tool: "describe_chart_source", ok: true, resource_id: "s1" },
      ],
    } as unknown as AssistantAnswer
    expect(dashboardProposalIds(answer)).toEqual([DP])
  })
})
