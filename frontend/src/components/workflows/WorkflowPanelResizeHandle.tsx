"use client"

import { useEffect, useRef, useState, type CSSProperties, type KeyboardEvent, type PointerEvent } from "react"

/**
 * How wide a workflow's step settings are beside its canvas. The user drags
 * the panel's left edge, or focuses it and presses the arrow keys, to read a
 * long query without scrolling sideways; a double-click puts it back. The
 * editor's grid reads the width and the canvas's room from CSS variables
 * (panelGridStyle), so CSS keeps the canvas at least CANVAS_MIN_WIDTH wide
 * whatever was kept, and the drag stops there.
 *
 * Only on a wide container (`@3xl`), where the panel sits beside the canvas:
 * below that it is under it, full width.
 */

/** The CSS variables the editor's grid reads: the panel's width, and the room it leaves the canvas. */
const PANEL_WIDTH_VAR = "--workflow-panel-width"
const CANVAS_RESERVE_VAR = "--workflow-canvas-reserve"

export const PANEL_WIDTH_DEFAULT = 340
export const PANEL_WIDTH_MIN = 280
export const PANEL_WIDTH_MAX = 960
/**
 * The canvas beside the panel never gets narrower than this: the room the
 * docked Ask rsync.ai panel leaves a page (PAGE_MIN_WIDTH in dock-width.ts).
 */
export const CANVAS_MIN_WIDTH = 480
/** The grid's gap-3, between the canvas and the panel. */
const GRID_GAP = 12
/** One arrow-key press. */
export const PANEL_WIDTH_STEP = 16

export const PANEL_WIDTH_STORAGE_KEY = "rsync-workflow-panel-width"

/**
 * The widest the panel may be in a grid this wide, leaving the canvas
 * CANVAS_MIN_WIDTH. A grid not laid out (0 wide) gets the fixed cap.
 */
export function maxPanelWidth(gridWidth: number): number {
  if (!(gridWidth > 0)) return PANEL_WIDTH_MAX
  return Math.max(PANEL_WIDTH_MIN, Math.min(PANEL_WIDTH_MAX, gridWidth - CANVAS_MIN_WIDTH - GRID_GAP))
}

/** The grid's CSS variables for a panel this wide. */
export function panelGridStyle(width: number): CSSProperties {
  return {
    [PANEL_WIDTH_VAR]: `${width}px`,
    [CANVAS_RESERVE_VAR]: `${CANVAS_MIN_WIDTH + GRID_GAP}px`,
  } as CSSProperties
}

export function clampPanelWidth(width: number, max: number): number {
  return Math.round(Math.min(Math.max(width, PANEL_WIDTH_MIN), Math.max(max, PANEL_WIDTH_MIN)))
}

/**
 * The width this browser last chose. Not clamped to the grid: a width kept
 * from a wider screen comes back when the window does.
 */
export function readStoredPanelWidth(): number {
  if (typeof window === "undefined") return PANEL_WIDTH_DEFAULT
  try {
    const stored = Number(window.localStorage.getItem(PANEL_WIDTH_STORAGE_KEY))
    return Number.isFinite(stored) && stored >= PANEL_WIDTH_MIN && stored <= PANEL_WIDTH_MAX
      ? Math.round(stored)
      : PANEL_WIDTH_DEFAULT
  } catch {
    return PANEL_WIDTH_DEFAULT
  }
}

export function storePanelWidth(width: number) {
  try {
    if (width === PANEL_WIDTH_DEFAULT) window.localStorage.removeItem(PANEL_WIDTH_STORAGE_KEY)
    else window.localStorage.setItem(PANEL_WIDTH_STORAGE_KEY, String(Math.round(width)))
  } catch {}
}

/** The step settings' left edge: a WAI-ARIA window splitter whose value is the panel's width. */
export function WorkflowPanelResizeHandle({
  width,
  onWidth,
  measureMax,
  controls,
}: {
  width: number
  onWidth: (width: number) => void
  /** The widest the panel may be now (maxPanelWidth of the grid's width). */
  measureMax: () => number
  controls: string
}) {
  const [max, setMax] = useState(PANEL_WIDTH_MAX)
  const [resizing, setResizing] = useState(false)
  /** Ends the drag in progress; null when none is. */
  const endDragRef = useRef<(() => void) | null>(null)
  /** Where that drag began, the cap then, and the width it has reached. */
  const dragRef = useRef({ x: 0, width: 0, max: PANEL_WIDTH_MAX, latest: 0 })

  // Leaving the page mid-drag: the page still gets its cursor back.
  useEffect(() => () => endDragRef.current?.(), [])

  const measure = () => {
    const m = measureMax()
    setMax(m)
    return m
  }

  const commit = (next: number) => {
    const fitted = clampPanelWidth(next, measure())
    onWidth(fitted)
    storePanelWidth(fitted)
  }

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    e.currentTarget.setPointerCapture(e.pointerId)
    dragRef.current = { x: e.clientX, width, max: measure(), latest: width }
    const { cursor, userSelect } = document.body.style
    document.body.style.cursor = "col-resize"
    document.body.style.userSelect = "none"
    endDragRef.current = () => {
      endDragRef.current = null
      document.body.style.cursor = cursor
      document.body.style.userSelect = userSelect
      setResizing(false)
      storePanelWidth(dragRef.current.latest)
    }
    setResizing(true)
  }

  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    if (!endDragRef.current) return
    // The panel grows leftwards, by as far as the pointer has moved left. Measured
    // from the press, so a press off the strip's centre does not make it jump.
    const d = dragRef.current
    d.latest = clampPanelWidth(d.width + d.x - e.clientX, d.max)
    onWidth(d.latest)
  }

  const endDrag = () => endDragRef.current?.()

  // The panel grows leftwards, so ArrowLeft widens it.
  const keyWidth = (key: string): number | null => {
    switch (key) {
      case "ArrowLeft":
        return width + PANEL_WIDTH_STEP
      case "ArrowRight":
        return width - PANEL_WIDTH_STEP
      case "Home":
        return PANEL_WIDTH_MIN
      case "End":
        return PANEL_WIDTH_MAX
      default:
        return null
    }
  }

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const next = keyWidth(e.key)
    if (next === null) return
    e.preventDefault()
    commit(next)
  }

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize step settings"
      aria-controls={controls}
      aria-valuemin={PANEL_WIDTH_MIN}
      aria-valuemax={max}
      aria-valuenow={width}
      aria-valuetext={`${width} pixels wide`}
      tabIndex={0}
      title="Drag to resize · double-click to reset"
      data-resizing={resizing || undefined}
      onFocus={measure}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      onLostPointerCapture={endDrag}
      onDoubleClick={() => commit(PANEL_WIDTH_DEFAULT)}
      onKeyDown={onKeyDown}
      className="group absolute inset-y-0 -left-3 z-10 hidden w-3 cursor-col-resize touch-none select-none focus-visible:outline-none @3xl:block"
    >
      <span
        aria-hidden
        className="pointer-events-none absolute inset-y-0 left-1/2 w-0.5 -translate-x-1/2 transition-colors group-hover:bg-violet-400 group-focus-visible:bg-violet-500 group-data-[resizing]:bg-violet-500"
      />
    </div>
  )
}
