import { authFetch } from "@/lib/api/auth-fetch"

/** One end of a pipeline, as the list endpoint returns it. */
export interface PipelineListEnd {
  id?: string
  name?: string
  connector_type?: string
}

/** The fields of a `GET /api/v1/pipelines` row that a picker needs. */
export interface PipelineListRow {
  id: string
  name: string
  source_connection?: PipelineListEnd | null
  destination_connection?: PipelineListEnd | null
  sync_mode?: string | null
  cdc_mode?: string | null
  /** The list's own status (pipelines.go pipelineDerivedStatusCaseSQL). */
  derived_status?: string | null
}

/**
 * The handler's own ceiling (pipelines.go ListPipelines). Past it the handler does
 * not clamp, it falls back to 25 — so asking for more would page more slowly.
 */
export const PIPELINE_PAGE_SIZE = 100

/** 5,000 pipelines. A workspace past that needs search on the server, not a longer loop. */
export const MAX_PIPELINE_PAGES = 50

export interface AllPipelines {
  pipelines: PipelineListRow[]
  /** True when the list stopped before the server said it was done. */
  truncated: boolean
}

/**
 * Every pipeline in the active workspace, newest first.
 *
 * `GET /api/v1/pipelines` with no `page` answers with the first 50 and nothing more,
 * which is how a picker built on it silently lost every older pipeline. This pages
 * instead, until the server says there is no next page. A page that adds no pipeline
 * not already seen also ends it: that is a server ignoring the page number, and asking
 * again would only return the same rows. Throws on any failed page, so the caller can
 * say the list did not load rather than show part of it as if it were all.
 */
export async function listAllPipelines(): Promise<AllPipelines> {
  const byId = new Map<string, PipelineListRow>()
  for (let page = 1; page <= MAX_PIPELINE_PAGES; page++) {
    const res = await authFetch(`/api/v1/pipelines?page=${page}&per_page=${PIPELINE_PAGE_SIZE}`, {
      cache: "no-store",
    })
    if (!res.ok) throw new Error(`pipeline list page ${page} returned ${res.status}`)
    const data = (await res.json()) as {
      pipelines?: (Partial<PipelineListRow> & { id?: unknown; name?: unknown })[]
      pagination?: { has_next?: boolean }
    }
    const before = byId.size
    for (const p of data.pipelines ?? []) {
      if (typeof p.id !== "string" || p.id === "" || byId.has(p.id)) continue
      byId.set(p.id, {
        id: p.id,
        name: (typeof p.name === "string" && p.name.trim()) || p.id,
        source_connection: p.source_connection ?? null,
        destination_connection: p.destination_connection ?? null,
        sync_mode: p.sync_mode ?? null,
        cdc_mode: p.cdc_mode ?? null,
        derived_status: p.derived_status ?? null,
      })
    }
    const hasNext = data.pagination?.has_next === true
    if (!hasNext) return { pipelines: [...byId.values()], truncated: false }
    if (byId.size === before) return { pipelines: [...byId.values()], truncated: true }
  }
  return { pipelines: [...byId.values()], truncated: true }
}

/**
 * The pipelines that read from or write to each connection, keyed by connection id.
 * A pipeline with the same connection at both ends is listed once under it.
 */
export function pipelinesByConnection(pipelines: PipelineListRow[]): Map<string, PipelineListRow[]> {
  const byConnection = new Map<string, PipelineListRow[]>()
  for (const p of pipelines) {
    const ends = new Set([p.source_connection?.id, p.destination_connection?.id])
    for (const id of ends) {
      if (!id) continue
      const list = byConnection.get(id)
      if (list) list.push(p)
      else byConnection.set(id, [p])
    }
  }
  return byConnection
}
