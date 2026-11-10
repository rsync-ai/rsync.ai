/**
 * How wide the docked Ask rsync.ai panel is. The user drags its left edge
 * (AssistantResizeHandle); the page beside it gives up the same width
 * (DashboardShell). Both read one CSS variable on <html>, so a drag repaints
 * them without re-rendering the chat.
 *
 * Only the docked panel on a wide screen (Tailwind `xl`, 1280 px) is resizable:
 * below that it is a fixed-width overlay, and Expand fills the page.
 */

/** The CSS variable both the panel and the page's right padding read. */
export const DOCK_WIDTH_VAR = "--assistant-dock-width"

export const ASSISTANT_DOCK_DEFAULT = 440
export const ASSISTANT_DOCK_MIN = 360
/** The page beside the panel never gets narrower than this. */
export const PAGE_MIN_WIDTH = 480
/** One arrow-key press. */
export const DOCK_WIDTH_STEP = 16

/** The sidebar's width: md:pl-16 collapsed, md:pl-64 open. */
const SIDEBAR_WIDTH = { collapsed: 64, open: 256 } as const

export const DOCK_WIDTH_STORAGE_KEY = "rsync-assistant-width"

/** The widest the panel may be in a window this wide, leaving the page PAGE_MIN_WIDTH. */
export function maxDockWidth(viewportWidth: number, sidebarCollapsed: boolean): number {
  const sidebar = sidebarCollapsed ? SIDEBAR_WIDTH.collapsed : SIDEBAR_WIDTH.open
  return Math.max(ASSISTANT_DOCK_MIN, viewportWidth - sidebar - PAGE_MIN_WIDTH)
}

export function clampDockWidth(width: number, max: number): number {
  return Math.round(Math.min(Math.max(width, ASSISTANT_DOCK_MIN), Math.max(max, ASSISTANT_DOCK_MIN)))
}

/**
 * The width this browser last chose. Not clamped to the window: a width kept
 * from a wider screen comes back when the window does.
 */
export function readStoredDockWidth(): number {
  if (typeof window === "undefined") return ASSISTANT_DOCK_DEFAULT
  try {
    const stored = Number(window.localStorage.getItem(DOCK_WIDTH_STORAGE_KEY))
    return Number.isFinite(stored) && stored >= ASSISTANT_DOCK_MIN ? Math.round(stored) : ASSISTANT_DOCK_DEFAULT
  } catch {
    return ASSISTANT_DOCK_DEFAULT
  }
}

export function storeDockWidth(width: number) {
  try {
    if (width === ASSISTANT_DOCK_DEFAULT) window.localStorage.removeItem(DOCK_WIDTH_STORAGE_KEY)
    else window.localStorage.setItem(DOCK_WIDTH_STORAGE_KEY, String(Math.round(width)))
  } catch {}
}
