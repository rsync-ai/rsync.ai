import { createRef } from "react"
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import type { ChartDraw, ChartRecord, ChartResult, ServerChartSettings } from "@/lib/api/charts"
import type { Dashboard } from "@/lib/api/dashboards"

// Downloads (dashboards-design.md § 8, D3). A drawn chart offers a PNG and an SVG
// image, a drawn table a CSV of the rows it read; a scorecard, a message, a break or a
// failed read offers nothing. A dashboard's header has Export PDF, which asks this
// site's PDF route with the filters picked on the page. Nothing here runs a query:
// each file is made from what is on screen, or by the route.

const io = vi.hoisted(() => ({
  downloadBlob: vi.fn(),
  chartImage: vi.fn(),
  chartPng: vi.fn(),
  getDashboard: vi.fn(),
  drawTile: vi.fn(),
  exportDashboardPdf: vi.fn(),
  toastError: vi.fn(),
}))
vi.mock("@/lib/export/download", async (actual) => ({
  ...(await actual<typeof import("@/lib/export/download")>()),
  downloadBlob: io.downloadBlob,
}))
vi.mock("@/lib/export/chart-image", () => ({ chartImage: io.chartImage, chartPng: io.chartPng }))
vi.mock("@/lib/api/dashboards", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/dashboards")>()),
  getDashboard: io.getDashboard,
  drawTile: io.drawTile,
  exportDashboardPdf: io.exportDashboardPdf,
}))
vi.mock("@/components/charts/EChartNoSSR", () => ({ EChartNoSSR: () => <div data-testid="echart" /> }))
vi.mock("@/components/dashboards/DashboardGridEditorNoSSR", () => ({ DashboardGridEditorNoSSR: () => null }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspaceRole: () => ({ meets: () => true, isLoading: false }) }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: io.toastError } }))

import { ChartExportMenu } from "@/components/charts/ChartExportMenu"
import { DashboardPage } from "@/components/dashboards/DashboardPage"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  vi.clearAllMocks()
  io.chartImage.mockReturnValue({ svg: "<svg>chart</svg>", width: 832, height: 460 })
  io.chartPng.mockResolvedValue(new Blob(["png"], { type: "image/png" }))
})

const result = (columns: [string, string][], rows: unknown[][]): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked: [],
  unfiltered: [],
  sql: "",
  read_at: "2026-10-06T04:41:00Z",
  execution_time_ms: 3,
})
const draw = (r: ChartResult, extra: Partial<ChartDraw> = {}): ChartDraw => ({ result: r, break: null, badges: null, ...extra })

const BAR: ServerChartSettings = { version: 2, type: "bar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } }
const BAR_DRAW = draw(result([["x", "the x"], ["v", "the measure"]], [["2026-10-01T00:00:00Z", 10]]))
const TABLE: ServerChartSettings = {
  version: 2,
  type: "table",
  roles: { columns: [{ column: "region" }, { column: "note" }], measure: [{ aggregate: "sum", column: "amount" }] },
}
const TABLE_DRAW = draw(
  result(
    [
      ["region", "table column region"],
      ["note", "table column note"],
      ["amount", "the measure"],
    ],
    [
      ["EU", "=HYPERLINK(1)", 1234.5],
      ["US, east", 'say "hi"', -7],
    ],
  ),
)
const CARD: ServerChartSettings = { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } }
const CARD_DRAW = draw(result([["count", "the measure"]], [[42]]))

async function openMenu(name: string) {
  fireEvent.keyDown(screen.getByRole("button", { name: `Download ${name}` }), { key: "Enter" })
  return screen.findAllByRole("menuitem")
}

/** Opens the dashboard's ⋯ menu and finds one of its items. */
async function openDashboardMenu(item: string) {
  fireEvent.keyDown(await screen.findByRole("button", { name: "Dashboard actions" }), { key: "Enter" })
  return screen.findByRole("menuitem", { name: item })
}

function renderMenu(name: string, settings: ServerChartSettings, d: ChartDraw | null) {
  return render(<ChartExportMenu name={name} settings={settings} draw={d} plot={createRef<HTMLElement>()} />)
}

describe("ChartExportMenu", () => {
  it("downloads a table as a CSV of the rows on screen, a formula defused", async () => {
    renderMenu("Revenue by region", TABLE, TABLE_DRAW)
    const items = await openMenu("Revenue by region")
    expect(items.map((i) => i.textContent)).toEqual(["CSV file"])
    fireEvent.click(items[0])

    await waitFor(() => expect(io.downloadBlob).toHaveBeenCalledTimes(1))
    const [blob, filename] = io.downloadBlob.mock.calls[0] as [Blob, string]
    expect(filename).toMatch(/^Revenue by region \d{4}-\d{2}-\d{2}\.csv$/)
    expect(blob.type).toBe("text/csv;charset=utf-8")
    const text = new TextDecoder("utf-8", { ignoreBOM: true }).decode(await blob.arrayBuffer())
    expect(text.split("\r\n")).toEqual(["﻿region,note,Sum of amount", "EU,'=HYPERLINK(1),1234.5", '"US, east","say ""hi""",-7', ""])
    expect(io.chartImage).not.toHaveBeenCalled()
  })

  it("downloads a chart as an SVG or a PNG of itself, its name above it", async () => {
    renderMenu("Swipes per day", BAR, BAR_DRAW)
    let items = await openMenu("Swipes per day")
    expect(items.map((i) => i.textContent)).toEqual(["PNG image", "SVG image"])
    fireEvent.click(items[1])
    await waitFor(() => expect(io.downloadBlob).toHaveBeenCalledTimes(1))
    const [svg, svgName] = io.downloadBlob.mock.calls[0] as [Blob, string]
    expect(svgName).toMatch(/^Swipes per day \d{4}-\d{2}-\d{2}\.svg$/)
    expect(svg.type).toBe("image/svg+xml;charset=utf-8")
    expect(new TextDecoder().decode(await svg.arrayBuffer())).toBe("<svg>chart</svg>")
    // No plot to measure in the test: the image takes the fallback size.
    const [title, settings, rows, size] = io.chartImage.mock.calls[0]
    expect(title).toBe("Swipes per day")
    expect(settings).toMatchObject({ type: "bar" })
    expect(rows).toHaveLength(1)
    expect(size).toEqual({ width: 800, height: 400 })

    items = await openMenu("Swipes per day")
    fireEvent.click(items[0])
    await waitFor(() => expect(io.downloadBlob).toHaveBeenCalledTimes(2))
    expect(io.chartPng).toHaveBeenCalledWith({ svg: "<svg>chart</svg>", width: 832, height: 460 })
    expect((io.downloadBlob.mock.calls[1] as [Blob, string])[1]).toMatch(/\.png$/)
  })

  it("takes the size the plot has on the page", async () => {
    const plot = document.createElement("div")
    const chart = document.createElement("div")
    chart.dataset.testid = "echart"
    Object.defineProperty(chart, "clientWidth", { value: 512 })
    Object.defineProperty(chart, "clientHeight", { value: 288 })
    plot.appendChild(chart)
    render(<ChartExportMenu name="Sized" settings={BAR} draw={BAR_DRAW} plot={{ current: plot }} />)
    fireEvent.click((await openMenu("Sized"))[1])
    await waitFor(() => expect(io.chartImage).toHaveBeenCalledTimes(1))
    expect(io.chartImage.mock.calls[0][3]).toEqual({ width: 512, height: 288 })
  })

  it("says so when the image cannot be made", async () => {
    io.chartPng.mockRejectedValueOnce(new Error("This browser could not make the PNG"))
    renderMenu("Swipes per day", BAR, BAR_DRAW)
    fireEvent.click((await openMenu("Swipes per day"))[0])
    await waitFor(() => expect(io.toastError).toHaveBeenCalledWith("Could not download the chart: This browser could not make the PNG"))
    expect(io.downloadBlob).not.toHaveBeenCalled()
  })

  it("offers nothing for a scorecard, a chart that did not draw, or one with no rows", () => {
    const { container } = renderMenu("Card", CARD, CARD_DRAW)
    renderMenu("Broken", BAR, draw(BAR_DRAW.result!, { break: { reason: "x", message: "gone" } }))
    renderMenu("Failed", BAR, { ...BAR_DRAW, result: null, error: "relation does not exist" })
    renderMenu("Empty", BAR, draw(result([["x", "the x"], ["v", "the measure"]], [])))
    renderMenu("Unread", BAR, null)
    expect(container.ownerDocument.querySelectorAll('[data-testid="chart-export"]')).toHaveLength(0)
  })
})

describe("a dashboard's downloads", () => {
  const record = (id: string, name: string, settings: ServerChartSettings): ChartRecord => ({
    id,
    name,
    source_state: "ok",
    settings,
    settings_version: 2,
    source_columns: ["day", "region"],
    visibility: "workspace",
    proposed_by: "",
    mine: true,
    created_at: "",
    updated_at: "",
  })
  const dashboard: Dashboard = {
    id: "d1",
    name: "Swipes",
    description: "",
    filters: [{ id: "f1", kind: "date_range", label: "Day", column: "day" }],
    tiles: [
      { id: "t1", kind: "chart", chart_id: "c1", chart: record("c1", "Swipes per day", BAR), x: 0, y: 0, w: 6, h: 4 },
      { id: "t2", kind: "chart", chart_id: "c2", chart: record("c2", "Count", CARD), x: 6, y: 0, w: 6, h: 4 },
    ],
    tile_count: 2,
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "",
    updated_at: "",
  }

  beforeEach(() => {
    io.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
    io.drawTile.mockImplementation((_d: string, tileId: string) =>
      Promise.resolve({ ...(tileId === "t1" ? BAR_DRAW : CARD_DRAW), chart: null, filters: { applied: [], ignored: [], unfiltered: [] } }),
    )
  })

  it("offers a download on a drawn chart tile and not on a scorecard tile", async () => {
    render(<DashboardPage dashboardId="d1" />)
    const bar = await screen.findByRole("region", { name: "Swipes per day" })
    await within(bar).findByRole("button", { name: "Download Swipes per day" })
    const card = screen.getByRole("region", { name: "Count" })
    await waitFor(() => expect(io.drawTile).toHaveBeenCalledTimes(2))
    expect(within(card).queryByRole("button", { name: /^Download/ })).toBeNull()
  })

  it("exports the PDF with the filters picked on the page, and saves it under the route's name", async () => {
    const pdf = new Blob(["%PDF-"], { type: "application/pdf" })
    io.exportDashboardPdf.mockResolvedValue({ blob: pdf, filename: "Swipes 2026-10-06.pdf" })
    render(<DashboardPage dashboardId="d1" />)
    fireEvent.click(await screen.findByRole("button", { name: "Custom" }))
    fireEvent.change(screen.getByLabelText("Day from"), { target: { value: "2026-10-01" } })
    fireEvent.change(screen.getByLabelText("Day to"), { target: { value: "2026-10-05" } })
    fireEvent.click(await openDashboardMenu("Export PDF"))

    await waitFor(() => expect(io.downloadBlob).toHaveBeenCalledWith(pdf, "Swipes 2026-10-06.pdf"))
    expect(io.exportDashboardPdf).toHaveBeenCalledWith("d1", { f1: { from: "2026-10-01", to: "2026-10-05" } })
  })

  it("says why the PDF was not made, and lets the viewer try again", async () => {
    io.exportDashboardPdf.mockRejectedValue(new Error("Could not load the dashboard in time"))
    render(<DashboardPage dashboardId="d1" />)
    fireEvent.click(await openDashboardMenu("Export PDF"))
    await waitFor(() => expect(io.toastError).toHaveBeenCalledWith("Could not load the dashboard in time"))
    expect(io.downloadBlob).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.getByRole("button", { name: "Dashboard actions" }).querySelector(".animate-spin")).toBeNull())
    expect(await openDashboardMenu("Export PDF")).not.toHaveAttribute("data-disabled")
  })
})
