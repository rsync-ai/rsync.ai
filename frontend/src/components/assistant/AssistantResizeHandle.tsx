"use client"

import { useEffect, useRef, type KeyboardEvent, type PointerEvent } from "react"

import {
  ASSISTANT_DOCK_DEFAULT,
  ASSISTANT_DOCK_MIN,
  DOCK_WIDTH_STEP,
  clampDockWidth,
  storeDockWidth,
} from "@/lib/assistant/dock-width"
import { useAssistantDockWidth } from "@/lib/assistant/use-dock-width"
import { useAssistantStore } from "@/lib/store/useAssistantStore"

/**
 * The docked panel's left edge. Drag it, or focus it and press the arrow keys,
 * to make Ask rsync.ai narrower or wider; a double-click puts it back to 440 px.
 * A WAI-ARIA window splitter whose value is the panel's width. Shown on wide
 * screens only (`xl`), where the panel sits beside the page.
 */
export function AssistantResizeHandle({ controls }: { controls: string }) {
  const { width, max } = useAssistantDockWidth()
  const setDockWidth = useAssistantStore((s) => s.setDockWidth)
  const setDockResizing = useAssistantStore((s) => s.setDockResizing)
  const resizing = useAssistantStore((s) => s.dockResizing)
  /** Ends the drag in progress; null when none is. */
  const endDragRef = useRef<(() => void) | null>(null)
  /** Where that drag began: the pointer's x, and the panel's width then. */
  const dragStartRef = useRef({ x: 0, width: 0 })

  // Escape closes the panel mid-drag: the page still gets its cursor back.
  useEffect(() => () => endDragRef.current?.(), [])

  const commit = (next: number) => {
    const fitted = clampDockWidth(next, max)
    setDockWidth(fitted)
    storeDockWidth(fitted)
  }

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    e.currentTarget.setPointerCapture(e.pointerId)
    dragStartRef.current = { x: e.clientX, width }
    const { cursor, userSelect } = document.body.style
    document.body.style.cursor = "col-resize"
    document.body.style.userSelect = "none"
    endDragRef.current = () => {
      endDragRef.current = null
      document.body.style.cursor = cursor
      document.body.style.userSelect = userSelect
      setDockResizing(false)
      storeDockWidth(useAssistantStore.getState().dockWidth)
    }
    setDockResizing(true)
  }

  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    if (!endDragRef.current) return
    // The panel grows leftwards, by as far as the pointer has moved left. Measured
    // from the press, not the window's edge: the grab strip is 12 px wide, and a
    // press off its centre must not make the panel jump.
    const start = dragStartRef.current
    setDockWidth(clampDockWidth(start.width + start.x - e.clientX, max))
  }

  const endDrag = () => endDragRef.current?.()

  // The panel grows leftwards, so ArrowLeft widens it.
  const keyWidth = (key: string): number | null => {
    switch (key) {
      case "ArrowLeft":
        return width + DOCK_WIDTH_STEP
      case "ArrowRight":
        return width - DOCK_WIDTH_STEP
      case "Home":
        return ASSISTANT_DOCK_MIN
      case "End":
        return max
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
      aria-label="Resize Ask rsync.ai"
      aria-controls={controls}
      aria-valuemin={ASSISTANT_DOCK_MIN}
      aria-valuemax={max}
      aria-valuenow={width}
      aria-valuetext={`${width} pixels wide`}
      tabIndex={0}
      title="Drag to resize · double-click to reset"
      data-resizing={resizing || undefined}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      onLostPointerCapture={endDrag}
      onDoubleClick={() => commit(ASSISTANT_DOCK_DEFAULT)}
      onKeyDown={onKeyDown}
      className="group absolute inset-y-0 -left-1.5 z-10 hidden w-3 cursor-col-resize touch-none select-none focus-visible:outline-none xl:block"
    >
      <span
        aria-hidden
        className="pointer-events-none absolute inset-y-0 left-1/2 w-0.5 -translate-x-1/2 transition-colors group-hover:bg-violet-400 group-focus-visible:bg-violet-500 group-data-[resizing]:bg-violet-500"
      />
    </div>
  )
}
