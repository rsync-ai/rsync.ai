"use client"

/**
 * The dashboard editor's grid (dashboards-design.md § 7.2): tiles dragged by their
 * handle on the 12-column grid and resized from their right edge, their bottom edge or
 * their corner, with the columns drawn while a tile moves; a tile's width menu sizes it
 * with a click (setWidth). Import DashboardGridEditorNoSSR, never this file, so
 * react-grid-layout loads only when an owner or admin edits a dashboard.
 */

import ReactGridLayout, { useContainerWidth, verticalCompactor, type Layout } from "react-grid-layout"
import "react-grid-layout/css/styles.css"
import "./dashboard-grid-editor.css"
import { useState, type ReactNode } from "react"

import { GRID_GAP, MIN_TILE_ROWS, ROW_HEIGHT, clampTile, type DraftTile } from "@/components/dashboards/dashboard-layout"
import { GRID_COLUMNS, MAX_TILE_HEIGHT } from "@/lib/api/dashboards"

export interface DashboardGridEditorProps {
  tiles: DraftTile[]
  /** Called with each tile's new place after a drag or a resize. */
  onLayout: (places: Record<string, { x: number; y: number; w: number; h: number }>) => void
  /** A tile's content; setWidth gives the tile a width in columns, as its width menu does. */
  renderTile: (tile: DraftTile, setWidth: (w: number) => void) => ReactNode
  /** The fewest rows a tile may be resized to; MIN_TILE_ROWS when absent. */
  minRows?: (tile: DraftTile) => number
}

/** The class of the element a tile is dragged by. */
export const DRAG_HANDLE = "dashboard-tile-handle"

export function DashboardGridEditor({ tiles, onLayout, renderTile, minRows = () => MIN_TILE_ROWS }: DashboardGridEditorProps) {
  const { width, containerRef, mounted } = useContainerWidth()
  // The columns show while a tile is dragged or resized, so it lands where it is meant to.
  const [moving, setMoving] = useState(false)
  const layout: Layout = tiles.map((t) => ({
    i: t.key,
    x: t.x,
    y: t.y,
    w: t.w,
    h: t.h,
    minW: 2,
    minH: minRows(t),
    maxH: MAX_TILE_HEIGHT,
  }))

  const changed = (next: Layout) => {
    const places: Record<string, { x: number; y: number; w: number; h: number }> = {}
    for (const item of next) places[item.i] = clampTile({ x: item.x, y: item.y, w: item.w, h: item.h })
    onLayout(places)
  }

  // A width from the menu: the tile stays where it is, moved left only to stay inside
  // the grid, and the tiles in its way move down, as when its edge is dragged.
  const setWidth = (key: string, w: number) => {
    const item = layout.find((l) => l.i === key)
    if (!item) return
    const sized = { ...item, w, x: Math.min(item.x, GRID_COLUMNS - w), static: true }
    changed(verticalCompactor.compact([sized, ...layout.filter((l) => l.i !== key)], GRID_COLUMNS))
  }

  return (
    <div ref={containerRef} className="dashboard-grid-editor relative w-full" data-testid="dashboard-grid-editor">
      {moving && (
        <div
          aria-hidden
          data-testid="dashboard-column-guides"
          className="dashboard-column-guides pointer-events-none absolute inset-0 grid"
          style={{ gridTemplateColumns: `repeat(${GRID_COLUMNS}, minmax(0, 1fr))`, columnGap: GRID_GAP }}
        >
          {Array.from({ length: GRID_COLUMNS }, (_, i) => (
            <div key={i} />
          ))}
        </div>
      )}
      {mounted && (
        <ReactGridLayout
          width={width}
          layout={layout}
          gridConfig={{ cols: GRID_COLUMNS, rowHeight: ROW_HEIGHT, margin: [GRID_GAP, GRID_GAP], containerPadding: [0, 0] }}
          dragConfig={{ enabled: true, handle: `.${DRAG_HANDLE}` }}
          resizeConfig={{ enabled: true, handles: ["e", "s", "se"] }}
          compactor={verticalCompactor}
          onLayoutChange={changed}
          onDragStart={() => setMoving(true)}
          onDragStop={() => setMoving(false)}
          onResizeStart={() => setMoving(true)}
          onResizeStop={() => setMoving(false)}
        >
          {tiles.map((t) => (
            <div key={t.key}>{renderTile(t, (w) => setWidth(t.key, w))}</div>
          ))}
        </ReactGridLayout>
      )}
    </div>
  )
}
