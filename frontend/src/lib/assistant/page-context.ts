/**
 * What the assistant's question is about, read from the page the user is on:
 * /pipelines/<id>, /connections/<id>, /executions/<id> (a run) or
 * /dashboards/<id>. Only a uuid counts, so /pipelines/new or
 * /connections/browse is no page.
 */

import type { AssistantPage } from "@/lib/api/assistant"

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"

const UUID_ONLY = new RegExp(`^${UUID}$`)

export function isUUID(value: string | null | undefined): boolean {
  return !!value && UUID_ONLY.test(value)
}

const ROUTES: Array<[RegExp, AssistantPage["kind"]]> = [
  [new RegExp(`^/pipelines/(${UUID})(?:/|$)`), "pipeline"],
  [new RegExp(`^/connections/(${UUID})(?:/|$)`), "connection"],
  [new RegExp(`^/executions/(${UUID})(?:/|$)`), "run"],
  [new RegExp(`^/dashboards/(${UUID})(?:/|$)`), "dashboard"],
]

export function pageFromPath(pathname: string | null | undefined): AssistantPage | null {
  if (!pathname) return null
  for (const [pattern, kind] of ROUTES) {
    const m = pattern.exec(pathname)
    if (m) return { kind, id: m[1].toLowerCase() }
  }
  return null
}

const NOUN: Record<AssistantPage["kind"], string> = {
  pipeline: "This pipeline",
  connection: "This connection",
  run: "This run",
  dashboard: "This dashboard",
}

/** How the "About" chip names a page. */
export function pageLabel(page: { kind: AssistantPage["kind"]; name?: string }): string {
  const noun = NOUN[page.kind]
  return page.name ? `${noun}: ${page.name}` : noun
}
