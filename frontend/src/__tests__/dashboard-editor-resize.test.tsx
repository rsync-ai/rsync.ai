import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen, within } from "@testing-library/react"
import type { ReactNode } from "react"

import { DashboardGridEditor, type DashboardGridEditorProps } from "@/components/dashboards/DashboardGridEditor"
import type { DraftTile } from "@/components/dashboards/dashboard-layout"
import type { ChartRecord } from "@/lib/api/charts"
import type { Dashboard } from "@/lib/api/dashboards"

// Sizing a tile in the dashboard editor (dashboards-design.md § 7.2). On prod a row of
// tiles ended in an empty right side: a tile resized only from a corner that showed on
// hover, and a new chart went under every tile at half the width. Now a tile resizes
// from its right edge, its bottom edge and its corner, the 12 columns show while it
// moves, its header has a width menu (a click, not a drag: WCAG 2.5.7), and a new chart
// takes the first space it fits.

interface GridProps {
  resizeConfig?: { enabled?: boolean; handles?: string[] }
  onDragStart?: () => void
  onDragStop?: () => void
  onResizeStart?: () => void
  onResizeStop?: () => void
  children?: ReactNode
}

// The grid itself is a stub; its compactor is the real one, which the width menu uses.
const grid = vi.hoisted(() => [] as GridProps[])
vi.mock("react-grid-layout", async (actual) => ({
  ...(await actual<typeof import("react-grid-layout")>()),
  default: (props: GridProps) => {
    grid.push(props)
    return <div>{props.children}</div>
  },
  useContainerWidth: () => ({ width: 1200, containerRef: { current: null }, mounted: true }),
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

beforeAll(() => {
  // The width menu is a Radix dropdown.
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  Element.prototype.hasPointerCapture ||= () => false
  Element.prototype.scrollIntoView ||= () => {}
})

beforeEach(() => {
  grid.length = 0
  editorProps.length = 0
  vi.clearAllMocks()
})

const tile = (key: string, x: number, y: number, w: number, h: number): DraftTile => ({ key, kind: "text", body: "", x, y, w, h })

describe("the editor's grid", () => {
  it("resizes a tile from its right edge, its bottom edge and its corner", () => {
    render(<DashboardGridEditor tiles={[tile("a", 0, 0, 6, 7)]} onLayout={() => {}} renderTile={() => null} />)
    expect(grid.at(-1)!.resizeConfig).toEqual({ enabled: true, handles: ["e", "s", "se"] })
  })

  it("shows the 12 columns only while a tile is resized or dragged", () => {
    render(<DashboardGridEditor tiles={[tile("a", 0, 0, 6, 7)]} onLayout={() => {}} renderTile={() => null} />)
    const guides = () => screen.queryByTestId("dashboard-column-guides")
    expect(guides()).toBeNull()

    act(() => grid.at(-1)!.onResizeStart!())
    expect(guides()!.children).toHaveLength(12)
    expect(guides()!.getAttribute("aria-hidden")).toBe("true")
    act(() => grid.at(-1)!.onResizeStop!())
    expect(guides()).toBeNull()

    act(() => grid.at(-1)!.onDragStart!())
    expect(guides()!.children).toHaveLength(12)
    act(() => grid.at(-1)!.onDragStop!())
    expect(guides()).toBeNull()
  })

  const editor = (tiles: DraftTile[]) => {
    const onLayout = vi.fn()
    const setWidth = new Map<string, (w: number) => void>()
    render(
      <DashboardGridEditor
        tiles={tiles}
        onLayout={onLayout}
        renderTile={(t, set) => {
          setWidth.set(t.key, set)
          return null
        }}
      />,
    )
    return { onLayout, setWidth: (key: string, w: number) => act(() => setWidth.get(key)!(w)) }
  }

  it("keeps a tile given a width where it is and moves the tiles in its way down", () => {
    const { onLayout, setWidth } = editor([tile("a", 0, 0, 3, 5), tile("b", 3, 0, 3, 5), tile("c", 6, 0, 6, 7), tile("t", 0, 7, 12, 8)])
    setWidth("a", 12)
    expect(onLayout).toHaveBeenLastCalledWith({
      a: { x: 0, y: 0, w: 12, h: 5 },
      b: { x: 3, y: 5, w: 3, h: 5 },
      c: { x: 6, y: 5, w: 6, h: 7 },
      t: { x: 0, y: 12, w: 12, h: 8 },
    })
  })

  it("moves a widened tile left to keep it inside the 12 columns", () => {
    const { onLayout, setWidth } = editor([tile("a", 0, 0, 9, 5), tile("b", 9, 0, 3, 5)])
    setWidth("b", 6)
    expect(onLayout).toHaveBeenLastCalledWith({ b: { x: 6, y: 0, w: 6, h: 5 }, a: { x: 0, y: 5, w: 9, h: 5 } })
  })

  it("lets the tiles under a narrowed tile move up into the space it leaves", () => {
    const { onLayout, setWidth } = editor([tile("a", 0, 0, 12, 5), tile("b", 6, 5, 6, 7)])
    setWidth("a", 6)
    expect(onLayout).toHaveBeenLastCalledWith({ a: { x: 0, y: 0, w: 6, h: 5 }, b: { x: 6, y: 0, w: 6, h: 7 } })
  })
})

describe("editing a dashboard", () => {
  const chart = (id: string): ChartRecord => ({
    id,
    name: id,
    source_state: "ok",
    settings: { version: 2, type: "bar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } },
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
      { id: "t1", kind: "chart", chart_id: "bar", chart: chart("bar"), x: 0, y: 0, w: 6, h: 7 },
      { id: "t2", kind: "text", body: "Notes", x: 0, y: 7, w: 12, h: 2 },
    ],
    tile_count: 2,
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "2026-10-06T00:00:00Z",
    updated_at: "2026-10-06T00:00:00Z",
  }

  const openEditor = async () => {
    api.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
    api.drawTile.mockReturnValue(new Promise(() => {}))
    api.listCharts.mockResolvedValue({ charts: [chart("bar"), chart("swipes by hour")], can_manage: true })
    render(<DashboardPage dashboardId="d1" />)
    fireEvent.click(await screen.findByRole("button", { name: /Edit/ }))
    await screen.findByTestId("grid-editor-stub")
  }

  it("puts a new chart, 4 columns wide, in the first space it fits", async () => {
    await openEditor()
    const add = screen.getByLabelText("Add a saved chart")
    await within(add).findByRole("option", { name: /swipes by hour/ })
    fireEvent.change(add, { target: { value: "swipes by hour" } })

    const added = editorProps.at(-1)!.tiles.find((t) => t.chart_id === "swipes by hour")
    expect(added).toMatchObject({ x: 6, y: 0, w: 4, h: 7 })
  })

  it("gives each tile a width menu: the full row, ¾, ½, ⅓ or ¼, with its own width checked", async () => {
    await openEditor()
    const { tiles, renderTile } = editorProps.at(-1)!
    const setWidth = vi.fn()
    render(<>{renderTile(tiles.find((t) => t.chart_id === "bar")!, setWidth)}</>)

    const trigger = screen.getByRole("button", { name: "Width of the tile bar, now ½ width" })
    fireEvent.keyDown(trigger, { key: "Enter" })
    const items = await screen.findAllByRole("menuitemradio")
    expect(items.map((i) => [i.getAttribute("aria-label") ?? i.textContent, i.getAttribute("aria-checked")])).toEqual([
      ["Full width", "false"],
      ["¾ width", "false"],
      ["½ width", "true"],
      ["⅓ width", "false"],
      ["¼ width", "false"],
    ])
    fireEvent.click(screen.getByRole("menuitemradio", { name: "Full width" }))
    expect(setWidth).toHaveBeenCalledWith(12)
  })

  it("wraps a tile's name to two lines on a phone, and shows it whole on hover", async () => {
    await openEditor()
    const { tiles, renderTile } = editorProps.at(-1)!
    render(<>{renderTile(tiles.find((t) => t.chart_id === "bar")!, () => {})}</>)

    const name = screen.getByTitle("bar")
    expect(name).toHaveTextContent("bar")
    expect(name).toHaveClass("line-clamp-2", "break-words", "@xl:block", "@xl:truncate")
    expect(name).not.toHaveClass("truncate")
  })

  it("names a width the menu does not offer in columns, and checks none", async () => {
    await openEditor()
    const { tiles, renderTile } = editorProps.at(-1)!
    render(<>{renderTile({ ...tiles.find((t) => t.kind === "text")!, w: 5 }, () => {})}</>)

    const trigger = screen.getByRole("button", { name: "Width of the tile Text, now 5 of 12 columns" })
    fireEvent.keyDown(trigger, { key: "Enter" })
    const items = await screen.findAllByRole("menuitemradio")
    expect(items.map((i) => i.getAttribute("aria-checked"))).toEqual(["false", "false", "false", "false", "false"])
  })
})
