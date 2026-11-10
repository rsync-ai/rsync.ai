"use client"

import { useLayoutEffect, useSyncExternalStore } from "react"

import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { useUIStore } from "@/lib/store/useUIStore"
import { DOCK_WIDTH_VAR, clampDockWidth, maxDockWidth } from "./dock-width"

function subscribeToResize(onChange: () => void) {
  window.addEventListener("resize", onChange)
  return () => window.removeEventListener("resize", onChange)
}

// clientWidth leaves out the scrollbar, as a `right-0` fixed panel does.
const viewportWidth = () => document.documentElement.clientWidth
const noViewport = () => 0

export function useViewportWidth(): number {
  return useSyncExternalStore(subscribeToResize, viewportWidth, noViewport)
}

/** The docked panel's width as shown: the user's choice, fitted to this window. */
export function useAssistantDockWidth(): { width: number; max: number } {
  const chosen = useAssistantStore((s) => s.dockWidth)
  const sidebarCollapsed = useUIStore((s) => s.sidebarCollapsed)
  const max = maxDockWidth(useViewportWidth(), sidebarCollapsed)
  return { width: clampDockWidth(chosen, max), max }
}

/**
 * Puts the docked width on <html> as DOCK_WIDTH_VAR, where the panel (portalled
 * to <body>) and the page's right padding both read it. A drag then repaints
 * the two without re-rendering the chat.
 */
export function useDockWidthVar() {
  const { width } = useAssistantDockWidth()
  useLayoutEffect(() => {
    document.documentElement.style.setProperty(DOCK_WIDTH_VAR, `${width}px`)
  }, [width])
  useLayoutEffect(
    () => () => {
      document.documentElement.style.removeProperty(DOCK_WIDTH_VAR)
    },
    []
  )
}
