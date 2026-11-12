import { beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen } from "@testing-library/react"

import { DashboardGridEditor, type DashboardGridEditorProps } from "@/components/dashboards/DashboardGridEditor"
import type { DraftTile } from "@/components/dashboards/dashboard-layout"
import type { ChartRecord } from "@/lib/api/charts"
import type { Dashboard } from "@/lib/api/dashboards"

// The editor keeps a scorecard tile at 3 rows or more (dashboards-design.md § 7.2):
// a 2-row tile has room for its title and one line, so on prod the number sat
// under the tile's scrollbar. Every other tile still goes down to 2 rows.

const layouts = vi.hoisted(() => [] as { i: string; minH?: number }[][])
vi.mock("react-grid-layout", () => ({
  default: ({ layout, children }: { layout: { i: string; minH?: number }[]; children: React.ReactNode }) => {
    layouts.push(layout)
    return <div>{children}</div>
  },
  useContainerWidth: () => ({ width: 1200, containerRef: { current: null }, mounted: true }),
  verticalCompactor: {},
}))

const editorProps = vi.hoisted(() => [] as DashboardGridEditorProps[])
vi.mock("@/components/dashboards/DashboardGridEditorNoSSR", () => ({
  DashboardGridEditorNoSSR: (props: DashboardGridEditorProps) => {
    editorProps.push(props)
    return <div data-testid="grid-editor-stub" />
  },
}))

const api = vi.hoisted(() => ({ getDashboard: vi.fn(), drawTile: vi.fn(), listCharts: vi.fn() }))
vi.mock("@/lib/api/dashboards", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/dashboards")>()),
  getDashboard: api.getDashboard,
  drawTile: api.drawTile,
}))
vi.mock("@/lib/api/charts", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/charts")>()),
  listCharts: api.listCharts,
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspaceRole: () => ({ meets: () => true, isLoading: false }) }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { DashboardPage } from "@/components/dashboards/DashboardPage"

beforeEach(() => {
  layouts.length = 0
  editorProps.length = 0
  vi.clearAllMocks()
})

const tile = (key: string, kind: "chart" | "text", chart_id?: string): DraftTile => ({ key, kind, chart_id, body: "", x: 0, y: 0, w: 3, h: 2 })

describe("the editor's grid", () => {
  it("gives each tile the fewest rows it is told, and 2 when it is told none", () => {
    const tiles = [tile("card", "chart", "c1"), tile("bar", "chart", "c2"), tile("note", "text")]
    render(<DashboardGridEditor tiles={tiles} onLayout={() => {}} renderTile={() => null} minRows={(t) => (t.key === "card" ? 3 : 2)} />)
    expect(layouts.at(-1)!.map((l) => [l.i, l.minH])).toEqual([
      ["card", 3],
      ["bar", 2],
      ["note", 2],
    ])

    render(<DashboardGridEditor tiles={tiles} onLayout={() => {}} renderTile={() => null} />)
    expect(layouts.at(-1)!.map((l) => l.minH)).toEqual([2, 2, 2])
  })
})

describe("editing a dashboard", () => {
  const chart = (id: string, type: "scorecard" | "bar"): ChartRecord => ({
    id,
    name: id,
    source_state: "ok",
    settings:
      type === "scorecard"
        ? { version: 2, type, roles: { measure: [{ aggregate: "count" }] } }
        : { version: 2, type, roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } },
    settings_version: 2,
    source_columns: ["day"],
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "2026-10-06T00:00:00Z",
    updated_at: "2026-10-06T00:00:00Z",
  })
  const dashboard: Dashboard = {
    id: "d1",
    name: "Swipes",
    description: "",
    filters: [],
    tiles: [
      { id: "t1", kind: "chart", chart_id: "card", chart: chart("card", "scorecard"), x: 0, y: 0, w: 3, h: 5 },
      { id: "t2", kind: "chart", chart_id: "bar", chart: chart("bar", "bar"), x: 3, y: 0, w: 6, h: 7 },
      { id: "t3", kind: "text", body: "Notes", x: 0, y: 7, w: 12, h: 2 },
    ],
    tile_count: 3,
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "2026-10-06T00:00:00Z",
    updated_at: "2026-10-06T00:00:00Z",
  }

  it("keeps a scorecard's tile at 3 rows and lets the others go to 2", async () => {
    api.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
    api.drawTile.mockReturnValue(new Promise(() => {}))
    api.listCharts.mockResolvedValue({ charts: [], can_manage: true })
    render(<DashboardPage dashboardId="d1" />)
    fireEvent.click(await screen.findByRole("button", { name: /Edit/ }))
    await screen.findByTestId("grid-editor-stub")

    const { tiles, minRows } = editorProps.at(-1)!
    expect(minRows).toBeTypeOf("function")
    expect(tiles.map((t) => [t.chart_id ?? t.kind, minRows!(t)])).toEqual([
      ["card", 3],
      ["bar", 2],
      ["text", 2],
    ])
  })
})
