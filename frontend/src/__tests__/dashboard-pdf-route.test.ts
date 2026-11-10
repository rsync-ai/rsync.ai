// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { ChartRecord } from "@/lib/api/charts"
import type { Dashboard } from "@/lib/api/dashboards"

// /proxy/dashboards/[id]/pdf (dashboards-design.md § 8, D3): it reads the dashboard
// and draws its tiles through the gateway as the viewer — their session, their CSRF
// pair, their workspace, their filters — and answers with the PDF. It refuses a
// request without the CSRF header, as the gateway would, before asking the gateway.

const hoisted = vi.hoisted(() => ({ jar: {} as Record<string, string> }))

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => (name in hoisted.jar ? { name, value: hoisted.jar[name] } : undefined),
  }),
}))

const ID = "df657dc5-60a9-43bb-8eb9-9924b9e23f15"
const CARD: ChartRecord = {
  id: "c1",
  name: "Orders this week",
  source_state: "ok",
  settings: { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } },
  settings_version: 2,
  source_columns: ["region"],
  visibility: "workspace",
  proposed_by: "",
  mine: true,
  created_at: "",
  updated_at: "",
}
const DASHBOARD: Dashboard = {
  id: ID,
  name: "Ops review: Q4 (EU)",
  description: "",
  filters: [{ id: "f2", kind: "values", label: "Region", column: "region" }],
  tiles: [
    { id: "t1", kind: "chart", chart_id: "c1", chart: CARD, x: 0, y: 0, w: 4, h: 3 },
    { id: "t2", kind: "text", body: "Weekly numbers.", x: 4, y: 0, w: 4, h: 3 },
  ],
  tile_count: 2,
  visibility: "workspace",
  proposed_by: "none",
  mine: true,
  created_at: "",
  updated_at: "",
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })

function stubGateway(dashboard: Response = json(200, { dashboard: DASHBOARD, can_manage: true })) {
  const f = vi.fn(async (url: string) => {
    if (url.endsWith(`/dashboards/${ID}`)) return dashboard
    return json(200, {
      chart: CARD,
      result: { columns: [{ alias: "count", role: "the measure" }], rows: [[42]], truncated: false, masked: [], unfiltered: [], sql: "", read_at: "", execution_time_ms: 1 },
      break: null,
      badges: null,
      filters: { applied: ["f2"], ignored: [], unfiltered: [] },
    })
  })
  vi.stubGlobal("fetch", f)
  return f
}

function post(body: unknown, headers: Record<string, string> = { "X-CSRF-Token": "csrf-1" }) {
  return new Request(`http://localhost/proxy/dashboards/${ID}/pdf`, { method: "POST", headers, body: JSON.stringify(body) })
}

async function call(req: Request, id = ID) {
  const { POST } = await import("@/app/proxy/dashboards/[id]/pdf/route")
  return POST(req, { params: Promise.resolve({ id }) })
}

beforeEach(() => {
  for (const k of Object.keys(hoisted.jar)) delete hoisted.jar[k]
  hoisted.jar.auth_token = "sess-1"
  hoisted.jar.csrf_token = "csrf-1"
  hoisted.jar.rsync_active_workspace_id = "ws-1"
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe("/proxy/dashboards/[id]/pdf", () => {
  it("refuses a request without the CSRF header, or with the wrong one, before asking the gateway", async () => {
    const f = stubGateway()
    expect((await call(post({}, {}))).status).toBe(403)
    expect((await call(post({}, { "X-CSRF-Token": "someone-elses" }))).status).toBe(403)
    expect(f).not.toHaveBeenCalled()
  })

  it("refuses a viewer who is not signed in", async () => {
    delete hoisted.jar.auth_token
    const f = stubGateway()
    expect((await call(post({}))).status).toBe(401)
    expect(f).not.toHaveBeenCalled()
  })

  it("asks the gateway as the viewer, with only the dashboard's own filters, and answers with the PDF", async () => {
    const f = stubGateway()
    const res = await call(post({ filters: { f2: { values: ["EU"] }, nope: { values: ["x"] } } }))
    expect(res.status).toBe(200)
    expect(res.headers.get("Content-Type")).toBe("application/pdf")
    expect(res.headers.get("Cache-Control")).toBe("no-store")
    const disposition = res.headers.get("Content-Disposition") ?? ""
    expect(disposition).toMatch(/^attachment; filename="ops-review-q4-eu-\d{4}-\d{2}-\d{2}\.pdf"; filename\*=UTF-8''Ops%20review%20Q4%20%28EU%29%20/)
    const bytes = Buffer.from(await res.arrayBuffer())
    expect(bytes.subarray(0, 5).toString()).toBe("%PDF-")

    expect(f).toHaveBeenCalledTimes(2)
    for (const [, init] of f.mock.calls as unknown as [string, RequestInit][]) {
      const headers = init.headers as Record<string, string>
      expect(headers.Cookie).toBe("auth_token=sess-1; csrf_token=csrf-1; rsync_active_workspace_id=ws-1")
      expect(headers["X-CSRF-Token"]).toBe("csrf-1")
    }
    const [drawUrl, drawInit] = f.mock.calls[1] as unknown as [string, RequestInit]
    expect(drawUrl).toMatch(new RegExp(`/api/v1/dashboards/${ID}/tiles/t1/draw$`))
    expect(drawInit.method).toBe("POST")
    expect(JSON.parse(String(drawInit.body))).toEqual({ filters: { f2: { values: ["EU"] } } })
  })

  it("passes the gateway's refusal on, with its status", async () => {
    stubGateway(json(404, { error: "not_found", message: "Dashboard not found" }))
    const res = await call(post({}))
    expect(res.status).toBe(404)
    expect(await res.json()).toEqual({ error: "Dashboard not found" })
  })

  it("answers 404 for an id that is not a dashboard id, without asking the gateway", async () => {
    const f = stubGateway()
    expect((await call(post({}), "../charts")).status).toBe(404)
    expect(f).not.toHaveBeenCalled()
  })
})
