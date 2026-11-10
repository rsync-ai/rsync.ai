// @vitest-environment node
import PDFDocument from "pdfkit"
import { afterEach, describe, expect, it, vi } from "vitest"

import { POST } from "@/app/api/internal/dashboards/pdf/route"
import type { ChartRecord, ServerChartSettings } from "@/lib/api/charts"
import type { DashboardTile } from "@/lib/api/dashboards"

// A workflow's dashboard as a PDF for its email (dashboards-design.md § 8, D3): the
// gateway posts the board's name, its tiles and each chart tile's answer, read as the
// run's user, holding the internal secret; the route answers with the PDF the board's
// Export PDF button makes, with no filters picked.

const SECRET = "unit-test-internal-secret-0123456789abcdef"

const record = (name: string, settings: ServerChartSettings): ChartRecord => ({
  id: name,
  name,
  source_state: "ok",
  settings,
  settings_version: 2,
  source_columns: [],
  visibility: "workspace",
  proposed_by: "",
  mine: true,
  created_at: "",
  updated_at: "",
})

const BAR = record("Orders by region", {
  version: 2,
  type: "bar",
  roles: { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount" }], series: [{ column: "region" }] },
})
const CARD = record("Orders this week", { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } })

const tile = (id: string, over: Partial<DashboardTile>): DashboardTile => ({ id, kind: "chart", x: 0, y: 0, w: 6, h: 6, ...over })

/** What the gateway posts (workflow_dashboard_step.go dashboardPDFRequest): results without sql or timings. */
function gatewayBody() {
  return {
    name: "Ops review",
    generated_at: "2026-10-07T09:00:00Z",
    tiles: [
      tile("bar", { chart: BAR, chart_id: BAR.id, x: 0, y: 0, w: 8, h: 7 }),
      tile("card", { chart: CARD, chart_id: CARD.id, x: 8, y: 0, w: 4, h: 3 }),
      tile("note", { kind: "text", body: "Weekly numbers for the ops review.", x: 8, y: 3, w: 4, h: 4 }),
      tile("hidden", { chart_id: "someone-elses", chart_state: "hidden", x: 0, y: 7, w: 6, h: 3 }),
      tile("broken", { chart: BAR, chart_id: BAR.id, x: 6, y: 7, w: 6, h: 3 }),
    ],
    draws: {
      bar: {
        draw: {
          chart: BAR,
          result: {
            columns: [
              { alias: "x", role: "the x" },
              { alias: "s", role: "the series" },
              { alias: "v", role: "the measure" },
            ],
            rows: [
              ["2026-10-01", "EU", "10"],
              ["2026-10-01", "US", "20"],
            ],
            truncated: false,
            masked: [],
            unfiltered: [],
          },
          break: null,
          badges: null,
          filters: { applied: [], ignored: [] },
        },
        failure: null,
      },
      card: {
        draw: {
          chart: CARD,
          result: { columns: [{ alias: "count", role: "the measure" }], rows: [[120]], truncated: false, masked: [], unfiltered: [] },
          break: null,
          badges: null,
          filters: { applied: [], ignored: [] },
        },
        failure: null,
      },
      broken: { draw: null, failure: "it took too long to read" },
    },
  }
}

function post(body: unknown, secret: string | null = SECRET): Request {
  const headers: Record<string, string> = { "Content-Type": "application/json" }
  if (secret !== null) headers["X-Internal-Secret"] = secret
  return new Request("http://frontend:3000/api/internal/dashboards/pdf", {
    method: "POST",
    headers,
    body: typeof body === "string" ? body : JSON.stringify(body),
  })
}

/** Every string the PDF writes, as dashboard-pdf.test.ts reads them. */
function spyText(): string[] {
  const written: string[] = []
  const text = PDFDocument.prototype.text
  vi.spyOn(PDFDocument.prototype, "text").mockImplementation(function (this: PDFKit.PDFDocument, ...args: unknown[]) {
    if (typeof args[0] === "string") written.push(args[0])
    return (text as (...a: unknown[]) => PDFKit.PDFDocument).apply(this, args)
  })
  return written
}

afterEach(() => {
  vi.unstubAllEnvs()
  vi.restoreAllMocks()
})

describe("the internal dashboard PDF route", () => {
  it("answers with the board's PDF: every tile, the one not drawn saying why, no filters", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const written = spyText()
    const resp = await POST(post(gatewayBody()))
    expect(resp.status).toBe(200)
    expect(resp.headers.get("Content-Type")).toBe("application/pdf")
    expect(resp.headers.get("Cache-Control")).toBe("no-store")
    const pdf = Buffer.from(await resp.arrayBuffer())
    expect(pdf.subarray(0, 5).toString("latin1")).toBe("%PDF-")
    expect(resp.headers.get("Content-Length")).toBe(String(pdf.length))

    for (const t of ["Ops review", "Orders by region", "Orders this week", "120", "Weekly numbers for the ops review."]) {
      expect(written).toContain(t)
    }
    expect(written).toContain("This chart is private to its author.")
    expect(written).toContain("This tile could not be drawn: it took too long to read")
    expect(written).toEqual(expect.arrayContaining(["EU", "US"]))
    expect(written.some((t) => t.includes("Generated 2026-10-07 09:00 UTC"))).toBe(true)
    expect(written.some((t) => t.startsWith("Filters:"))).toBe(false)
  })

  it("prints a chart tile the gateway sent no answer for as not drawn", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const written = spyText()
    const body = gatewayBody()
    const draws: Record<string, unknown> = { ...body.draws }
    delete draws.bar
    const resp = await POST(post({ ...body, draws }))
    expect(resp.status).toBe(200)
    expect(written).toContain("This tile could not be drawn: no answer")
  })

  it("answers no one when the secret is not set", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", "  ")
    const resp = await POST(post(gatewayBody()))
    expect(resp.status).toBe(503)
    expect(await resp.json()).toEqual({ error: expect.stringContaining("INTERNAL_SERVICE_SECRET") })
  })

  it.each([
    ["no secret", null],
    ["a wrong secret", "unit-test-internal-secret-0123456789abcdeX"],
    ["an empty secret", ""],
  ])("refuses a caller with %s before reading the body", async (_, secret) => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post("not json at all", secret))
    expect(resp.status).toBe(401)
    expect(await resp.json()).toEqual({ error: "Not an internal caller" })
  })

  it.each([
    ["a body that is not JSON", "{"],
    ["a body that is not an object", [1, 2]],
    ["no name", { ...gatewayBody(), name: 7 }],
    ["no time", { ...gatewayBody(), generated_at: "yesterday" }],
    ["a tile with no place", { ...gatewayBody(), tiles: [{ id: "t", kind: "chart" }] }],
    ["a tile of no kind", { ...gatewayBody(), tiles: [tile("t", { kind: "image" as DashboardTile["kind"] })] }],
    ["draws that are a list", { ...gatewayBody(), draws: [] }],
    ["a draw with neither", { ...gatewayBody(), draws: { bar: {} } }],
    ["a failure that is not text", { ...gatewayBody(), draws: { bar: { failure: 5 } } }],
    ["a result with no rows", { ...gatewayBody(), draws: { bar: { draw: { chart: BAR, result: { columns: [] } } } } }],
  ])("refuses %s", async (_, body) => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post(body))
    expect(resp.status).toBe(400)
    expect(await resp.json()).toEqual({ error: expect.any(String) })
  })

  it("answers 500 without the cause when the PDF fails, and logs the cause", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const logged = vi.spyOn(console, "error").mockImplementation(() => {})
    vi.resetModules()
    vi.doMock("@/lib/export/dashboard-pdf", async (actual) => ({
      ...(await actual<typeof import("@/lib/export/dashboard-pdf")>()),
      dashboardPdf: () => Promise.reject(new Error("Inter-Regular.ttf: no such file")),
    }))
    try {
      const route = await import("@/app/api/internal/dashboards/pdf/route")
      const resp = await route.POST(post(gatewayBody()))
      expect(resp.status).toBe(500)
      expect(await resp.json()).toEqual({ error: "The PDF could not be built" })
      expect(logged).toHaveBeenCalledWith("Workflow dashboard PDF: building the PDF failed:", expect.any(Error))
    } finally {
      vi.doUnmock("@/lib/export/dashboard-pdf")
      vi.resetModules()
    }
  })
})
