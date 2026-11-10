import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { dashboardChangeIds } from "@/components/assistant/AssistantThread"
import { changeRows, DashboardChangeReview } from "@/components/dashboards/DashboardChangeReview"
import { authFetch } from "@/lib/api/auth-fetch"
import type { AssistantAnswer } from "@/lib/api/assistant"
import type { ChartGrain } from "@/lib/api/charts"
import type { DashboardChange, DashboardChangeSide } from "@/lib/api/dashboards"
import { API_ENDPOINTS } from "@/lib/config/api"
import { onDashboardChanged } from "@/lib/events/dashboardChanged"

// A change Ask rsync.ai proposed to one dashboard tile (plan_dashboard_change,
// dashboards-design.md § 7.6). The server owns every rule: who may apply, the
// settings' checks, whether the chart is copied, the version it keeps. What is
// pinned here is what only the card can get wrong:
//   - loading the card changes nothing;
//   - the table shows the tile now and after, marks what changes, and leaves
//     out a row neither side has;
//   - the card says whether Apply copies the chart or changes it in place;
//   - it promises an undo only when History can give one: a version keeps each
//     tile's chart id, never a chart's settings (§ 7.7), so a copy or a size
//     change is undone and a chart changed in place is not;
//   - Apply is one POST, only while the change is open, by someone who may, and
//     says so to the dashboard open for it (dashboard-change-refresh.test.tsx);
//   - a refusal that closes the change reloads it, so the card says what it is now;
//   - Ask rsync.ai shows a card for each stored change, never for a refused call.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as unknown as Mock
const CHANGE = "dddddddd-0000-4000-8000-0000000000c1"
const DASH = "dddddddd-0000-4000-8000-0000000000a1"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function side(overrides: Partial<DashboardChangeSide> = {}): DashboardChangeSide {
  return {
    name: "Orders by region",
    type: "bar",
    settings: { version: 2, type: "bar", roles: { x: [{ column: "region" }], measure: [{ aggregate: "count" }] } },
    x: 0,
    y: 0,
    w: 6,
    h: 7,
    ...overrides,
  }
}

function change(overrides: Partial<DashboardChange> = {}): DashboardChange {
  return {
    id: CHANGE,
    dashboard_id: DASH,
    dashboard_name: "Sales",
    tile_id: "t1",
    chart_id: "c1",
    before: side(),
    after: side({
      type: "line",
      settings: {
        version: 2,
        type: "line",
        roles: { x: [{ column: "region" }], measure: [{ aggregate: "sum", column: "amount", color: "series-2" }] },
      },
      w: 12,
    }),
    moves: [{ tile_id: "t2", from_y: 0, y: 7 }],
    summary: "A line of the amount by region, full width.",
    chart_changed: true,
    size_changed: true,
    shared_elsewhere: 0,
    copy: false,
    source_id: "s1",
    source_name: "Daily orders",
    status: "proposed",
    expired: false,
    stale: false,
    copied: false,
    proposed_by: "assistant",
    created_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    url: `https://app.example/dashboards/${DASH}?change=${CHANGE}`,
    ...overrides,
  }
}

function route(opts: { gets?: Response[]; apply?: Response; canManage?: boolean } = {}) {
  const gets = [...(opts.gets ?? [])]
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.DASHBOARD_CHANGES.GET(CHANGE) && method === "GET") {
      return Promise.resolve(gets.shift() ?? json(200, { change: change(), can_manage: opts.canManage ?? true }))
    }
    if (url === API_ENDPOINTS.DASHBOARD_CHANGES.APPLY(CHANGE) && method === "POST") {
      return Promise.resolve(opts.apply ?? json(200, { dashboard_id: DASH, chart_id: "c1", copied: false }))
    }
    return Promise.resolve(json(500, { error: `unexpected ${method} ${url}` }))
  })
}

const posts = () => mockFetch.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === "POST")
const gets = () => mockFetch.mock.calls.filter(([, init]) => !(init as RequestInit | undefined)?.method)

beforeEach(() => {
  mockFetch.mockReset()
})

describe("changeRows", () => {
  it("marks what changes and leaves out a row neither side has", () => {
    const rows = changeRows(change().before, change().after)
    const byLabel = Object.fromEntries(rows.map((r) => [r.label, r]))
    expect(rows.map((r) => r.label)).toEqual(["Title", "Chart", "X", "Measure", "Colour", "Size", "Place"])
    expect(byLabel.Title.changed).toBe(false)
    expect(byLabel.Chart).toMatchObject({ before: "Bar", after: "Line", changed: true })
    expect(byLabel.Measure).toMatchObject({ before: "Count of rows", after: "Sum of amount", changed: true })
    expect(byLabel.Colour).toMatchObject({ before: "—", after: { color: "series-2" }, changed: true })
    expect(byLabel.Size).toMatchObject({ before: "6 of 12 columns, 7 rows high", after: "12 of 12 columns, 7 rows high" })
    expect(byLabel.Place).toMatchObject({ before: "Column 1, row 1", changed: false })
  })

  it("shows an x's date grain, so a change of grain alone is a change", () => {
    const by = (grain?: ChartGrain): DashboardChangeSide => ({
      ...side(),
      settings: { version: 2, type: "bar", roles: { x: [grain ? { column: "day", grain } : { column: "day" }], measure: [{ aggregate: "count" }] } },
    })
    const row = (b: DashboardChangeSide, a: DashboardChangeSide) => changeRows(b, a).find((r) => r.label === "X")
    expect(row(by(), by("month"))).toMatchObject({ before: "day", after: "day by month", changed: true })
    expect(row(by("week"), by("month"))).toMatchObject({ before: "day by week", after: "day by month", changed: true })
    expect(row(by("week"), by("week"))).toMatchObject({ changed: false })
  })
})

describe("DashboardChangeReview", () => {
  it("shows the tile now and after, and loading it sends nothing", async () => {
    route()
    render(<DashboardChangeReview changeId={CHANGE} />)
    expect(screen.getByRole("status")).toHaveTextContent("Loading the proposed change…")
    const card = await screen.findByRole("region", { name: "Proposed dashboard change" })
    expect(card).toHaveTextContent("Proposed change to Sales")
    expect(card).toHaveTextContent("A line of the amount by region, full width.")
    expect(screen.getByText("Waiting for you")).toBeInTheDocument()
    const chart = screen.getByRole("row", { name: /^Chart/ })
    expect(within(chart).getAllByRole("cell").map((c) => c.textContent)).toEqual(["Bar", "Line (changed)"])
    const colour = screen.getByRole("row", { name: /^Colour/ })
    expect(within(colour).getAllByRole("cell").map((c) => c.textContent)).toEqual(["—", "Orange (changed)"])
    expect(card).toHaveTextContent("1 tile below it moves down to make room.")
    expect(card).toHaveTextContent("Apply changes the chart in place: it is on no other dashboard.")
    expect(card).toHaveTextContent("keeps the dashboard as it is now in its History")
    expect(screen.getByRole("link", { name: "Open dashboard" })).toHaveAttribute(
      "href",
      `/dashboards/${DASH}?change=${CHANGE}`,
    )
    expect(posts()).toHaveLength(0)
  })

  it("says Apply copies a chart another dashboard shows, and that the copy was made", async () => {
    route({
      gets: [json(200, { change: change({ shared_elsewhere: 2, copy: true }), can_manage: true })],
      apply: json(200, { dashboard_id: DASH, chart_id: "c9", copied: true }),
    })
    render(<DashboardChangeReview changeId={CHANGE} />)
    const card = await screen.findByRole("region", { name: "Proposed dashboard change" })
    expect(card).toHaveTextContent(
      "The chart is on 2 other dashboards too, so Apply copies it for this tile and those dashboards keep theirs.",
    )
    expect(card).not.toHaveTextContent("changes the chart in place")
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }))
    expect(await screen.findByText(/This tile now has its own copy of the chart/)).toBeInTheDocument()
  })

  // KI-DASHBOARD-CHANGE-CARD-PROMISES-UNDO: on prod a chart changed in place stayed
  // a horizontal bar after a Restore while the card had said "can be undone".
  it.each([
    {
      what: "a chart changed in place",
      over: { size_changed: false, moves: [], after: side({ name: "Orders by area" }) },
      undone: false,
    },
    { what: "a chart changed in place with its size", over: {}, undone: false },
    { what: "a copied chart", over: { shared_elsewhere: 1, copy: true }, undone: true },
    { what: "a size change only", over: { chart_changed: false, after: side({ w: 12 }) }, undone: true },
  ])("promises an undo from History only when it gives one: $what", async ({ over, undone }) => {
    route({ gets: [json(200, { change: change(over), can_manage: true })] })
    render(<DashboardChangeReview changeId={CHANGE} />)
    const card = await screen.findByRole("region", { name: "Proposed dashboard change" })
    expect(card).toHaveTextContent("Apply keeps the dashboard as it is now in its History")
    if (undone) {
      expect(card).toHaveTextContent("so the change can be undone.")
      expect(card).not.toHaveTextContent("not this chart's settings")
    } else {
      expect(card).toHaveTextContent("but not this chart's settings: a Restore will not change the chart back.")
      expect(card).not.toHaveTextContent("can be undone")
    }
  })

  it("applies with one POST, says so to the open dashboard, then offers no Apply", async () => {
    route()
    const announced = vi.fn()
    const stop = onDashboardChanged(announced)
    render(<DashboardChangeReview changeId={CHANGE} />)
    const apply = await screen.findByRole("button", { name: "Apply change" })
    fireEvent.click(apply)
    fireEvent.click(apply)
    await waitFor(() => expect(announced).toHaveBeenCalledTimes(1))
    stop()
    expect(announced).toHaveBeenCalledWith(DASH)
    expect(posts().map(([u]) => u)).toEqual([API_ENDPOINTS.DASHBOARD_CHANGES.APPLY(CHANGE)])
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
    expect(screen.getByText("Applied")).toBeInTheDocument()
  })

  it("gives a person who cannot manage no Apply", async () => {
    route({ canManage: false })
    render(<DashboardChangeReview changeId={CHANGE} />)
    expect(await screen.findByText(/Only workspace owners and admins can apply/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it.each([
    ["applied", { status: "applied" }, "Applied", "This change was applied."],
    ["expired", { expired: true }, "Expired", /expired 24 hours after it was proposed/],
    ["out of date", { stale: true }, "Out of date", /saved after this change was proposed/],
  ] as const)("says a change that is %s and offers no Apply", async (_, overrides, badge, text) => {
    route({ gets: [json(200, { change: change(overrides), can_manage: true })] })
    render(<DashboardChangeReview changeId={CHANGE} />)
    expect(await screen.findByText(text)).toBeInTheDocument()
    expect(screen.getByText(badge)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it("reloads after a refusal that closes the change, and says why", async () => {
    route({
      gets: [
        json(200, { change: change(), can_manage: true }),
        json(200, { change: change({ stale: true }), can_manage: true }),
      ],
      apply: json(409, { error: "The dashboard was saved after this change was proposed.", code: "dashboard_changed" }),
    })
    render(<DashboardChangeReview changeId={CHANGE} />)
    fireEvent.click(await screen.findByRole("button", { name: "Apply change" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("The dashboard was saved after this change was proposed.")
    expect(await screen.findByText("Out of date")).toBeInTheDocument()
    expect(gets()).toHaveLength(2)
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it("keeps Apply after a refusal that leaves the change open", async () => {
    route({ apply: json(422, { error: "The column amount is not in the source's result." }) })
    render(<DashboardChangeReview changeId={CHANGE} />)
    fireEvent.click(await screen.findByRole("button", { name: "Apply change" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("The column amount is not in the source's result.")
    expect(gets()).toHaveLength(1)
    expect(screen.getByRole("button", { name: "Apply change" })).toBeEnabled()
  })

  it("says a change for someone else was not found, without Retry", async () => {
    route({ gets: [json(404, { error: "Not found" })] })
    render(<DashboardChangeReview changeId={CHANGE} />)
    expect(await screen.findByRole("alert")).toHaveTextContent("This proposed change was not found.")
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull()
  })

  it("leaves out the dashboard link on the dashboard itself", async () => {
    route()
    render(<DashboardChangeReview changeId={CHANGE} hideOpenLink />)
    await screen.findByRole("region", { name: "Proposed dashboard change" })
    expect(screen.queryByRole("link", { name: "Open dashboard" })).toBeNull()
  })
})

describe("Ask rsync.ai's dashboard change cards", () => {
  it("shows a card for each stored change, never for a refused call", () => {
    const answer = {
      answer: "Proposed.",
      looked_at: [
        { tool: "plan_dashboard_change", ok: true, resource_id: CHANGE },
        { tool: "plan_dashboard_change", ok: true, resource_id: CHANGE },
        { tool: "plan_dashboard_change", ok: false, resource_id: "refused" },
        { tool: "plan_dashboard_change", ok: true },
        { tool: "plan_dashboard", ok: true, resource_id: "a-draft" },
        { tool: "get_dashboard", ok: true, resource_id: DASH },
      ],
    } as unknown as AssistantAnswer
    expect(dashboardChangeIds(answer)).toEqual([CHANGE, CHANGE])
  })
})
