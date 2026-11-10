// @vitest-environment node
import path from "node:path"

import { Resvg } from "@resvg/resvg-js"
import { afterEach, describe, expect, it, vi } from "vitest"

import { POST } from "@/app/api/internal/charts/png/route"
import { chartSpec, NOT_STATIC } from "@/components/charts/chart-registry"
import { BOX_ROLES, PRICE_ROLES, ROLE_SECOND_MEASURE, levelRole } from "@/components/charts/saved-chart"
import type { ChartResult, ServerChartSettings } from "@/lib/api/charts"
import { emailChartImage, emailChartPng, EMAIL_PLOT, NotPlotted, PLOTTED_TYPES, SCALE } from "@/lib/export/email-chart"
import { FONT_DIR } from "@/lib/export/dashboard-pdf"

// A workflow's chart as a PNG for its email (dashboards-design.md § 8, D3): the gateway
// posts the chart's name, settings and the rows it read as the run's user, holding the
// internal secret; the route answers with Download PNG's drawing, 600 px wide at twice
// the pixels, its letters in the Inter files and no system font.

const SECRET = "unit-test-internal-secret-0123456789abcdef"

const result = (columns: [alias: string, role: string][], rows: unknown[][], masked: string[] = []): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked,
  unfiltered: [],
  sql: "",
  read_at: "",
  execution_time_ms: 0,
})

const XY: [string, string][] = [
  ["x", "the x"],
  ["s", "the series"],
  ["v", "the measure"],
]
const XY_ROWS = ["2026-10-01", "2026-10-02", "2026-10-03"].flatMap((d, i) => [
  [d, "EU", String(10 + i)],
  [d, "US", String(20 - i)],
])
const BAR: ServerChartSettings = {
  version: 2,
  type: "bar",
  roles: { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount" }], series: [{ column: "region" }] },
}
const CARD: ServerChartSettings = { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } }

/** What the gateway posts (workflow_chart_step.go chartPNGRequest): the result without sql or timings. */
function gatewayBody(name: string, settings: ServerChartSettings, r: ChartResult) {
  return { name, settings, result: { columns: r.columns, rows: r.rows, truncated: r.truncated, masked: r.masked, unfiltered: r.unfiltered } }
}

function post(body: unknown, secret: string | null = SECRET): Request {
  const headers: Record<string, string> = { "Content-Type": "application/json" }
  if (secret !== null) headers["X-Internal-Secret"] = secret
  return new Request("http://frontend:3000/api/internal/charts/png", {
    method: "POST",
    headers,
    body: typeof body === "string" ? body : JSON.stringify(body),
  })
}

const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

/** A PNG's size from its IHDR chunk. */
const pngSize = (png: Buffer) => ({ width: png.readUInt32BE(16), height: png.readUInt32BE(20) })

afterEach(() => {
  vi.unstubAllEnvs()
  vi.restoreAllMocks()
})

describe("the internal chart PNG route", () => {
  it("answers a bar chart with its picture, 600 px wide at twice the pixels", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS))))
    expect(resp.status).toBe(200)
    expect(resp.headers.get("Content-Type")).toBe("image/png")
    expect(resp.headers.get("Cache-Control")).toBe("no-store")
    const png = Buffer.from(await resp.arrayBuffer())
    expect(png.subarray(0, 8).equals(PNG_SIGNATURE)).toBe(true)
    const image = emailChartImage("Orders by region", BAR, result(XY, XY_ROWS))
    expect(image.width).toBe(600)
    expect(pngSize(png)).toEqual({ width: 600 * SCALE, height: image.height * SCALE })
  })

  it("reads its own secret without the spaces around it, as the gateway reads its own", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", ` ${SECRET}\n`)
    const resp = await POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS))))
    expect(resp.status).toBe(200)
  })

  it("answers no one when the secret is not set", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", "  ")
    const resp = await POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS))))
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
    ["no name", { settings: BAR, result: gatewayBody("x", BAR, result(XY, XY_ROWS)).result }],
    ["settings of no chart type", { ...gatewayBody("x", BAR, result(XY, XY_ROWS)), settings: { type: "pie" } }],
    ["no result", { name: "x", settings: BAR }],
    ["columns without roles", { ...gatewayBody("x", BAR, result(XY, XY_ROWS)), result: { ...gatewayBody("x", BAR, result(XY, XY_ROWS)).result, columns: [{ alias: "x" }] } }],
    ["a row that is not a list", { ...gatewayBody("x", BAR, result(XY, XY_ROWS)), result: { ...gatewayBody("x", BAR, result(XY, XY_ROWS)).result, rows: [{ x: 1 }] } }],
    ["no masked list", { ...gatewayBody("x", BAR, result(XY, XY_ROWS)), result: { ...gatewayBody("x", BAR, result(XY, XY_ROWS)).result, masked: null } }],
  ])("refuses %s", async (_, body) => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post(body))
    expect(resp.status).toBe(400)
    expect(await resp.json()).toEqual({ error: expect.any(String) })
  })

  // The gateway upgrades a chart to version 2 before it posts (charts.AsVersion2): a version 1
  // chart here is a gateway bug, said so, never a picture drawn from fields it cannot read.
  it.each([
    ["version 1 settings", { type: "bar", x: { column: "day" }, measure: { aggregate: "sum", column: "amount" }, series: { column: "region" } }],
    ["version 2 without roles", { version: 2, type: "bar" }],
    ["roles at no version", { type: "bar", roles: BAR.roles }],
  ])("refuses %s", async (_, settings) => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post({ ...gatewayBody("x", BAR, result(XY, XY_ROWS)), settings }))
    expect(resp.status).toBe(400)
    expect(await resp.json()).toEqual({ error: "settings must be version 2, by role" })
  })

  it("refuses a scorecard, which an email shows as its number", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post(gatewayBody("Orders this week", CARD, result([["n", "the measure"]], [[42]]))))
    expect(resp.status).toBe(422)
    expect(await resp.json()).toEqual({ error: "A scorecard chart goes in the message as text, not as a picture." })
  })

  it("refuses a chart whose measure is masked, with the chart page's words", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const resp = await POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS, ["v"]))))
    expect(resp.status).toBe(422)
    expect(await resp.json()).toEqual({ error: "This chart's measure is masked for you, so it is not drawn." })
  })

  // dashboards-design.md § 14.5: a type the registry marks not static is drawn only in the
  // app. Every type is static today, so the test marks bar not static for its length.
  it("refuses a type the server does not draw, saying it is drawn only in rsync.ai", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const bar = chartSpec("bar")!
    bar.static = false
    try {
      const resp = await POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS))))
      expect(resp.status).toBe(422)
      expect(await resp.json()).toEqual({ error: NOT_STATIC })
    } finally {
      bar.static = true
    }
  })

  it("answers 500 without the cause when the drawing fails, and logs the cause", async () => {
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET)
    const logged = vi.spyOn(console, "error").mockImplementation(() => {})
    vi.resetModules()
    vi.doMock("@/lib/export/email-chart", async (actual) => ({
      ...(await actual<typeof import("@/lib/export/email-chart")>()),
      emailChartPng: () => {
        throw new Error("Inter-Regular.ttf: no such file")
      },
    }))
    try {
      const route = await import("@/app/api/internal/charts/png/route")
      const resp = await route.POST(post(gatewayBody("Orders by region", BAR, result(XY, XY_ROWS))))
      expect(resp.status).toBe(500)
      expect(await resp.json()).toEqual({ error: "The chart could not be drawn" })
      expect(logged).toHaveBeenCalledWith("Workflow chart PNG: drawing failed:", expect.any(Error))
    } finally {
      vi.doUnmock("@/lib/export/email-chart")
      vi.resetModules()
    }
  })
})

describe("the email chart's drawing", () => {
  it("is Download PNG's layout: the name, the plot at the email's width, the legend", () => {
    const image = emailChartImage("Orders by region", BAR, result(XY, XY_ROWS))
    expect(image.svg).toContain(">Orders by region</text>")
    expect(image.svg).toContain(`width="${EMAIL_PLOT.width}"`)
    expect(image.svg).toContain(">EU</text>")
    expect(image.svg).toContain(">US</text>")
    // The print theme: paper, whatever the reader's screen is.
    expect(image.svg).toContain('<rect width="600"')
    expect(image.svg).toContain('fill="#ffffff"')
  })

  it("names itself for a screen reader: a title and an aria label, escaped", () => {
    const image = emailChartImage(`Orders <EU> & "US"`, BAR, result(XY, XY_ROWS))
    expect(image.svg).toMatch(/^<svg [^>]*role="img" aria-label="Orders &lt;EU&gt; &amp; &quot;US&quot;"[^>]*><title>Orders &lt;EU&gt; &amp; &quot;US&quot;<\/title>/)
  })

  it("plots every type in the registry but a scorecard, a table and a pivot table, as the gateway's chartPlotted does", () => {
    expect(PLOTTED_TYPES).toEqual([
      "bar",
      "hbar",
      "line",
      "area",
      "pie",
      "donut",
      "waterfall",
      "funnel",
      "histogram",
      "heatmap",
      "scatter",
      "bubble",
      "treemap",
      "sunburst",
      "sankey",
      "calendar",
      "bullet",
      "boxplot",
      "candlestick",
      "range",
      "map",
    ])
  })

  it("draws its letters from the Inter files: without them the same picture has no text", () => {
    const image = emailChartImage("Orders by region", BAR, result(XY, XY_ROWS))
    const drawn = emailChartPng("Orders by region", BAR, result(XY, XY_ROWS))
    const fontless = new Resvg(image.svg, {
      fitTo: { mode: "zoom", value: SCALE },
      font: { loadSystemFonts: false, fontFiles: [], defaultFontFamily: "Inter" },
      logLevel: "off",
    })
      .render()
      .asPng()
    expect(pngSize(drawn)).toEqual(pngSize(fontless))
    expect(drawn.equals(fontless)).toBe(false)
    expect(FONT_DIR).toBe(path.join(process.cwd(), "src/assets/fonts"))
  })

  it.each(["hbar", "line", "area"] as const)("draws a %s chart", (type) => {
    const png = emailChartPng("Orders by region", { ...BAR, type }, result(XY, XY_ROWS))
    expect(png.subarray(0, 8).equals(PNG_SIGNATURE)).toBe(true)
    expect(pngSize(png).width).toBe(600 * SCALE)
  })

  // The second tier (§ 14.8): each draws as it does on the chart page, in resvg.
  const SECOND_TIER: [string, ServerChartSettings, ChartResult][] = [
    [
      "treemap",
      { version: 2, type: "treemap", roles: { rows: [{ column: "region" }, { column: "channel" }], measure: [{ aggregate: "sum", column: "amount" }] } },
      result([["region", levelRole(1)], ["channel", levelRole(2)], ["amount", "the measure"]], [["EU", "web", "320"], ["US", "web", "300"], ["US", "store", "270"]]),
    ],
    [
      "sunburst",
      { version: 2, type: "sunburst", roles: { rows: [{ column: "region" }, { column: "channel" }], measure: [{ aggregate: "sum", column: "amount" }] } },
      result([["region", levelRole(1)], ["channel", levelRole(2)], ["amount", "the measure"]], [["EU", "web", "320"], ["US", "web", "300"], ["US", "store", "270"]]),
    ],
    [
      "sankey",
      { version: 2, type: "sankey", roles: { x: [{ column: "from" }], y: [{ column: "to" }], measure: [{ aggregate: "count" }] } },
      result([["from", "the x"], ["to", "the y"], ["count", "the measure"]], [["visit", "cart", "50"], ["cart", "paid", "20"]]),
    ],
    [
      "calendar",
      { version: 2, type: "calendar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } },
      result([["day", "the x"], ["count", "the measure"]], [["2026-10-01T00:00:00Z", "3"], ["2026-10-02T00:00:00Z", "5"]]),
    ],
    [
      "bullet",
      { version: 2, type: "bullet", target: 1000, roles: { measure: [{ aggregate: "sum", column: "amount" }] } },
      result([["amount", "the measure"]], [["800"]]),
    ],
    [
      "boxplot",
      { version: 2, type: "boxplot", roles: { x: [{ column: "plan" }], y: [{ column: "amount" }] } },
      result([["plan", "the x"], ...BOX_ROLES.map((r, i): [string, string] => [`rsync_box_${i}`, r])], [["free", "10", "25", "40", "60", "150"]]),
    ],
    [
      "candlestick",
      { version: 2, type: "candlestick", roles: { x: [{ column: "day", grain: "month" }], y: ["open", "high", "low", "close"].map((column) => ({ column })) } },
      result([["day", "the x"], ...PRICE_ROLES.map((r, i): [string, string] => [`rsync_price_${i}`, r])], [["2026-09-01T00:00:00Z", "100", "130", "95", "120"]]),
    ],
    [
      "range",
      {
        version: 2,
        type: "range",
        roles: { x: [{ column: "day" }], measure: [{ aggregate: "min", column: "amount" }, { aggregate: "max", column: "amount" }] },
      },
      result([["day", "the x"], ["amount", "the measure"], ["amount_2", ROLE_SECOND_MEASURE]], [["2026-10-01", "40", "120"], ["2026-10-02", "55", "160"]]),
    ],
    [
      "map",
      { version: 2, type: "map", map: "world", roles: { x: [{ column: "country" }], measure: [{ aggregate: "sum", column: "amount" }] } },
      result([["country", "the x"], ["amount", "the measure"]], [["FR", "320"], ["US", "300"], ["JP", "270"]]),
    ],
  ]

  it.each(SECOND_TIER)("draws a %s chart", (_, settings, r) => {
    const png = emailChartPng("Orders", settings, r)
    expect(png.subarray(0, 8).equals(PNG_SIGNATURE)).toBe(true)
    expect(pngSize(png).width).toBe(600 * SCALE)
  })

  it("draws a type of the second tier for each one the registry has", () => {
    expect(SECOND_TIER.map(([type]) => type)).toEqual(PLOTTED_TYPES.slice(PLOTTED_TYPES.indexOf("treemap")))
  })

  it("does not draw a table or a pivot table", () => {
    expect(() => emailChartImage("Every order", { version: 2, type: "table", roles: { columns: [{ column: "id" }] } }, result([["id", "table column id"]], [["1"]]))).toThrow(NotPlotted)
    const pivot: ServerChartSettings = { version: 2, type: "pivot", roles: { rows: [{ column: "region" }], measure: [{ aggregate: "count" }] } }
    expect(() => emailChartImage("Orders by region", pivot, result(XY, XY_ROWS))).toThrow("A pivot chart goes in the message as text, not as a picture.")
  })
})
