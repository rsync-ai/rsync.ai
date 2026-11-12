/**
 * Same-tab signal that a workflow was drafted or changed outside the page that
 * lists it. Ask rsync.ai's panel sits beside the Workflows list and has no handle
 * on it, so without this a draft the panel made, or a draft it turned on or
 * applied a change to, was missing or stale in the list until a reload
 * (2026-10-02). The list reads itself again.
 */
export const WORKFLOW_CHANGED_EVENT = "rsync:workflow-changed"

export function notifyWorkflowChanged(workflowId: string): void {
  if (typeof window === "undefined") return
  try {
    window.dispatchEvent(new CustomEvent(WORKFLOW_CHANGED_EVENT, { detail: { workflowId } }))
  } catch {
    // CustomEvent unavailable in exotic environments; non-fatal.
  }
}

export function onWorkflowChanged(handler: (workflowId: string) => void): () => void {
  if (typeof window === "undefined") return () => {}
  const listener = (evt: Event) => {
    const id = (evt as CustomEvent<{ workflowId?: string }>).detail?.workflowId
    if (id) handler(id)
  }
  window.addEventListener(WORKFLOW_CHANGED_EVENT, listener)
  return () => window.removeEventListener(WORKFLOW_CHANGED_EVENT, listener)
}
