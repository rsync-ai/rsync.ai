import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"

import { AssistantResizeHandle } from "@/components/assistant/AssistantResizeHandle"
import { DashboardShell } from "@/components/layout/DashboardShell"
import {
  ASSISTANT_DOCK_DEFAULT,
  ASSISTANT_DOCK_MIN,
  DOCK_WIDTH_STORAGE_KEY,
  DOCK_WIDTH_VAR,
  clampDockWidth,
  maxDockWidth,
  readStoredDockWidth,
  storeDockWidth,
} from "@/lib/assistant/dock-width"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { useUIStore } from "@/lib/store/useUIStore"

// The docked Ask rsync.ai panel's width, which the user drags (Databricks-style):
//   - its left edge is a window splitter — pointer drag, arrow keys, Home/End,
//     double-click back to 440 px;
//   - it never goes under 360 px, and never leaves the page under 480 px;
//   - the page's right padding is the panel's width, through one CSS variable,
//     and stops animating while the edge is dragged;
//   - the chosen width is kept in this browser and comes back on the next load.

const nav = vi.hoisted(() => ({ pathname: "/" as string | null }))
vi.mock("next/navigation", () => ({ usePathname: () => nav.pathname }))

// jsdom has no PointerEvent, so fireEvent.pointer* would arrive without clientX.
beforeAll(() => {
  if (typeof window.PointerEvent === "undefined") {
    class PointerEventShim extends MouseEvent {
      pointerId: number
      constructor(type: string, init: PointerEventInit = {}) {
        super(type, init)
        this.pointerId = init.pointerId ?? 1
      }
    }
    window.PointerEvent = PointerEventShim as unknown as typeof PointerEvent
  }
})

let viewport = 1440
function setViewport(width: number) {
  viewport = width
  window.dispatchEvent(new Event("resize"))
}

beforeEach(() => {
  viewport = 1440
  Object.defineProperty(document.documentElement, "clientWidth", { configurable: true, get: () => viewport })
  try {
    localStorage.clear()
  } catch {}
  useUIStore.setState({ sidebarCollapsed: false })
  useAssistantStore.setState({ open: true, expanded: false, dockWidth: ASSISTANT_DOCK_DEFAULT, dockResizing: false })
})

afterEach(() => {
  act(() => useAssistantStore.setState({ open: false, expanded: false, dockResizing: false }))
  document.body.style.cursor = ""
  document.body.style.userSelect = ""
})

const dockVar = () => document.documentElement.style.getPropertyValue(DOCK_WIDTH_VAR)

// The handle as AssistantPanel mounts it: only while the panel is open.
function DockedPanel() {
  const open = useAssistantStore((s) => s.open)
  if (!open) return null
  return (
    <aside id="panel">
      <AssistantResizeHandle controls="panel" />
    </aside>
  )
}

function renderDocked() {
  render(
    <>
      <DashboardShell>page</DashboardShell>
      <DockedPanel />
    </>
  )
  return { main: screen.getByRole("main"), edge: screen.getByRole("separator", { name: "Resize Ask rsync.ai" }) }
}

describe("dock width limits", () => {
  it("leaves the page 480 px beside the sidebar, and never goes under 360 px", () => {
    expect(maxDockWidth(1440, false)).toBe(1440 - 256 - 480)
    expect(maxDockWidth(1440, true)).toBe(1440 - 64 - 480)
    expect(maxDockWidth(1000, false)).toBe(ASSISTANT_DOCK_MIN)
    expect(clampDockWidth(100, 704)).toBe(ASSISTANT_DOCK_MIN)
    expect(clampDockWidth(9000, 704)).toBe(704)
    expect(clampDockWidth(500.4, 704)).toBe(500)
  })

  it("reads back only a usable stored width", () => {
    expect(readStoredDockWidth()).toBe(ASSISTANT_DOCK_DEFAULT)
    localStorage.setItem(DOCK_WIDTH_STORAGE_KEY, "620")
    expect(readStoredDockWidth()).toBe(620)
    // Kept even past this window's limit: it fits again on a wider screen.
    localStorage.setItem(DOCK_WIDTH_STORAGE_KEY, "1500")
    expect(readStoredDockWidth()).toBe(1500)
    for (const junk of ["", "abc", "200", "-5", "NaN", "Infinity"]) {
      localStorage.setItem(DOCK_WIDTH_STORAGE_KEY, junk)
      expect(readStoredDockWidth()).toBe(ASSISTANT_DOCK_DEFAULT)
    }
    storeDockWidth(ASSISTANT_DOCK_DEFAULT)
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBeNull()
  })

  it("a new page load starts at the width this browser kept", async () => {
    localStorage.setItem(DOCK_WIDTH_STORAGE_KEY, "620")
    vi.resetModules()
    const fresh = await import("@/lib/store/useAssistantStore")
    expect(fresh.useAssistantStore.getState().dockWidth).toBe(620)
  })
})

describe("the page beside the docked panel", () => {
  it("pads by the panel's width, fitted to the window and the sidebar", () => {
    const { main } = renderDocked()
    expect(main.className).toContain("xl:pr-(--assistant-dock-width)")
    expect(dockVar()).toBe("440px")

    act(() => useAssistantStore.getState().setDockWidth(600))
    expect(dockVar()).toBe("600px")

    act(() => useAssistantStore.getState().setDockWidth(2000))
    expect(dockVar()).toBe("704px")
    act(() => useUIStore.setState({ sidebarCollapsed: true }))
    expect(dockVar()).toBe("896px")
    // A narrower window takes the panel down with it; the choice itself is kept.
    act(() => setViewport(1280))
    expect(dockVar()).toBe(`${1280 - 64 - 480}px`)
    expect(useAssistantStore.getState().dockWidth).toBe(2000)
  })
})

describe("dragging the panel's edge", () => {
  it("follows the pointer, stops the padding animation, and keeps the width", () => {
    const { main, edge } = renderDocked()
    expect(main.className).toContain("transition-[padding]")

    fireEvent.pointerDown(edge, { button: 0, pointerId: 1, clientX: 1000 })
    expect(main.className).not.toContain("transition-[padding]")
    expect(document.body.style.cursor).toBe("col-resize")

    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 900 })
    expect(dockVar()).toBe("540px")
    expect(edge).toHaveAttribute("aria-valuenow", "540")
    // Not saved mid-drag.
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBeNull()

    fireEvent.pointerUp(edge, { pointerId: 1, clientX: 900 })
    expect(main.className).toContain("transition-[padding]")
    expect(document.body.style.cursor).toBe("")
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBe("540")

    // A move with no button down is not a drag.
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 700 })
    expect(dockVar()).toBe("540px")
  })

  it("does not jump when grabbed off-centre: the panel moves by what the pointer moves", () => {
    const { edge } = renderDocked()
    // The grab strip is 12 px wide; a press 5 px right of the panel's edge...
    fireEvent.pointerDown(edge, { button: 0, pointerId: 1, clientX: 1005 })
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 1005 })
    expect(dockVar()).toBe("440px")
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 885 })
    expect(dockVar()).toBe("560px")
    fireEvent.pointerUp(edge, { pointerId: 1 })
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBe("560")
  })

  it("stops at 360 px and at the page's 480 px", () => {
    const { edge } = renderDocked()
    fireEvent.pointerDown(edge, { button: 0, pointerId: 1, clientX: 1000 })
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 100 })
    expect(dockVar()).toBe("704px")
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 1430 })
    expect(dockVar()).toBe("360px")
    fireEvent.pointerUp(edge, { pointerId: 1 })
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBe("360")
  })

  it("ignores a right-button press", () => {
    const { edge } = renderDocked()
    fireEvent.pointerDown(edge, { button: 2, pointerId: 1, clientX: 1000 })
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 800 })
    expect(dockVar()).toBe("440px")
    expect(useAssistantStore.getState().dockResizing).toBe(false)
  })

  it("closing the panel mid-drag gives the page its cursor and animation back", () => {
    const { main, edge } = renderDocked()
    fireEvent.pointerDown(edge, { button: 0, pointerId: 1, clientX: 1000 })
    fireEvent.pointerMove(edge, { pointerId: 1, clientX: 950 })
    expect(document.body.style.cursor).toBe("col-resize")
    // Escape closes the panel, and the handle goes with it.
    act(() => useAssistantStore.getState().close())
    expect(screen.queryByRole("separator")).toBeNull()
    expect(document.body.style.cursor).toBe("")
    expect(document.body.style.userSelect).toBe("")
    expect(useAssistantStore.getState().dockResizing).toBe(false)
    expect(main.className).toContain("transition-[padding]")
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBe("490")
  })
})

describe("the edge from the keyboard", () => {
  it("arrows step 16 px, Home and End go to the limits, double-click resets", () => {
    const { edge } = renderDocked()
    expect(edge).toHaveAttribute("tabindex", "0")
    expect(edge).toHaveAttribute("aria-valuemin", "360")
    expect(edge).toHaveAttribute("aria-valuemax", "704")
    expect(edge).toHaveAttribute("aria-valuenow", "440")

    fireEvent.keyDown(edge, { key: "ArrowLeft" })
    expect(edge).toHaveAttribute("aria-valuenow", "456")
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBe("456")
    fireEvent.keyDown(edge, { key: "ArrowRight" })
    fireEvent.keyDown(edge, { key: "ArrowRight" })
    expect(edge).toHaveAttribute("aria-valuenow", "424")
    fireEvent.keyDown(edge, { key: "End" })
    expect(dockVar()).toBe("704px")
    fireEvent.keyDown(edge, { key: "Home" })
    expect(dockVar()).toBe("360px")
    // Other keys pass through.
    expect(fireEvent.keyDown(edge, { key: "Enter" })).toBe(true)

    fireEvent.doubleClick(edge)
    expect(dockVar()).toBe("440px")
    expect(localStorage.getItem(DOCK_WIDTH_STORAGE_KEY)).toBeNull()
  })
})
