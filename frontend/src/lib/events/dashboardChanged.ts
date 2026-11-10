/**
 * Same-tab signal that a change was applied to a dashboard (dashboards-design.md
 * § 7.6). Ask rsync.ai's panel sits beside the page and has no handle on it, so
 * without this a change applied from the panel was saved while the dashboard open
 * behind it kept drawing the tile as it was (2026-10-06). The open dashboard the
 * change is for reads itself again.
 */
export const DASHBOARD_CHANGED_EVENT = "rsync:dashboard-changed"

export function notifyDashboardChanged(dashboardId: string): void {
  if (typeof window === "undefined") return
  try {
    window.dispatchEvent(new CustomEvent(DASHBOARD_CHANGED_EVENT, { detail: { dashboardId } }))
  } catch {
    // CustomEvent unavailable in exotic environments; non-fatal.
  }
}

export function onDashboardChanged(handler: (dashboardId: string) => void): () => void {
  if (typeof window === "undefined") return () => {}
  const listener = (evt: Event) => {
    const id = (evt as CustomEvent<{ dashboardId?: string }>).detail?.dashboardId
    if (id) handler(id)
  }
  window.addEventListener(DASHBOARD_CHANGED_EVENT, listener)
  return () => window.removeEventListener(DASHBOARD_CHANGED_EVENT, listener)
}
