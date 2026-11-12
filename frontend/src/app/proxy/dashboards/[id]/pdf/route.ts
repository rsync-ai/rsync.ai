// A dashboard as a PDF (dashboards-design.md § 8, D3). Under /proxy/, not /api/, so
// Traefik sends it to Next.js rather than the api-gateway. It reads the dashboard and
// draws each chart tile through the gateway exactly as the page does — as the viewer,
// with their session, workspace and the filters they picked — and lays the answers out
// on A4 (lib/export/dashboard-pdf.ts). Nothing is stored; the gateway is unchanged.
import { cookies } from "next/headers"
import { NextResponse } from "next/server"

import { activePicks, readingOrder } from "@/components/dashboards/dashboard-layout"
import { asDraw } from "@/lib/api/charts"
import type { Dashboard, DashboardFilterValue, DashboardTile, TileDraw } from "@/lib/api/dashboards"
import { API_GATEWAY_URL, API_GATEWAY_URL_INTERNAL, getApiUrl } from "@/lib/config/api"
import { dashboardPdf, filterLines, pdfTileOf } from "@/lib/export/dashboard-pdf"
import { exportFilename } from "@/lib/export/filename"
import { activeWorkspaceCookiePair } from "@/lib/workspace/server-workspace"

export const dynamic = "force-dynamic"
export const runtime = "nodejs"

const API_URL = getApiUrl(API_GATEWAY_URL, API_GATEWAY_URL_INTERNAL)

/** Tiles read at once, as on the page (DashboardPage DRAW_CONCURRENCY). */
const DRAW_CONCURRENCY = 4
/** One tile's draw; past it the tile prints as not drawn. */
const TILE_TIMEOUT_MS = 45_000
/** The whole export, under the proxy's 100 s; a tile not drawn by then prints as not drawn. */
const EXPORT_TIMEOUT_MS = 85_000

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const refuse = (status: number, error: string) => NextResponse.json({ error }, { status })

async function gatewayError(resp: Response, fallback: string): Promise<string> {
  try {
    const data = (await resp.json()) as { error?: unknown; message?: unknown }
    if (typeof data.message === "string" && data.message) return data.message
    if (typeof data.error === "string" && data.error) return data.error
  } catch {
    // Not JSON: the fallback says enough.
  }
  return fallback
}

export async function POST(request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  if (!UUID.test(id)) return refuse(404, "Dashboard not found")

  const cookieStore = await cookies()
  // The gateway's session cookie (auth.go setAuthCookie), not a name of our own.
  const session = cookieStore.get("auth_token")?.value
  if (!session) return refuse(401, "Sign in to export this dashboard")
  // The gateway's CSRF double-submit (csrf_middleware.go), checked here first: this
  // route spends the viewer's queries, so a page on another site may not start it.
  const csrf = cookieStore.get("csrf_token")?.value?.trim()
  const csrfHeader = request.headers.get("x-csrf-token")?.trim()
  if (!csrf || !csrfHeader || csrf !== csrfHeader) return refuse(403, "Missing or invalid CSRF token")

  let body: { filters?: unknown }
  try {
    body = (await request.json()) as { filters?: unknown }
  } catch {
    return refuse(400, "The request body must be JSON")
  }
  const picked =
    body.filters && typeof body.filters === "object" && !Array.isArray(body.filters)
      ? (body.filters as Record<string, DashboardFilterValue>)
      : {}

  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    Accept: "application/json",
    Cookie: [`auth_token=${session}`, `csrf_token=${csrf}`, activeWorkspaceCookiePair(cookieStore)].filter(Boolean).join("; "),
    "X-CSRF-Token": csrf,
  }
  const deadline = AbortSignal.any([AbortSignal.timeout(EXPORT_TIMEOUT_MS), request.signal])

  let dashboard: Dashboard
  try {
    const resp = await fetch(`${API_URL}/api/v1/dashboards/${encodeURIComponent(id)}`, { headers, signal: deadline, cache: "no-store" })
    if (!resp.ok) return refuse(resp.status, await gatewayError(resp, "Could not load the dashboard"))
    const data = (await resp.json()) as { dashboard?: Dashboard }
    if (!data.dashboard || typeof data.dashboard.id !== "string") return refuse(502, "Unexpected dashboard response")
    dashboard = data.dashboard
  } catch (err) {
    console.error("Dashboard PDF: reading the dashboard failed:", err)
    return refuse(504, "Could not load the dashboard in time")
  }

  const filters = Array.isArray(dashboard.filters) ? dashboard.filters : []
  const tiles = Array.isArray(dashboard.tiles) ? dashboard.tiles : []
  const sent = activePicks(filters, picked)

  // Each chart tile the viewer may see, drawn as the page draws it.
  const draws = new Map<string, { draw: TileDraw | null; failure: string | null }>()
  const queue = readingOrder(tiles.filter((t): t is DashboardTile => t.kind === "chart" && !!t.chart))
  const drawOne = async (tile: DashboardTile) => {
    try {
      const resp = await fetch(
        `${API_URL}/api/v1/dashboards/${encodeURIComponent(id)}/tiles/${encodeURIComponent(tile.id)}/draw`,
        {
          method: "POST",
          headers,
          body: JSON.stringify({ filters: sent }),
          signal: AbortSignal.any([AbortSignal.timeout(TILE_TIMEOUT_MS), deadline]),
          cache: "no-store",
        },
      )
      if (!resp.ok) {
        draws.set(tile.id, { draw: null, failure: await gatewayError(resp, `the gateway answered ${resp.status}`) })
        return
      }
      const data = (await resp.json()) as Partial<TileDraw>
      draws.set(tile.id, {
        draw: {
          ...asDraw(data),
          chart: data.chart ?? null,
          filters: {
            applied: Array.isArray(data.filters?.applied) ? data.filters.applied : [],
            ignored: Array.isArray(data.filters?.ignored) ? data.filters.ignored : [],
            unfiltered: Array.isArray(data.filters?.unfiltered) ? data.filters.unfiltered : [],
          },
        },
        failure: null,
      })
    } catch (err) {
      const timedOut = err instanceof Error && (err.name === "TimeoutError" || err.name === "AbortError")
      draws.set(tile.id, { draw: null, failure: timedOut ? "it took too long to read" : "the gateway could not be reached" })
    }
  }
  const worker = async () => {
    for (let t = queue.shift(); t; t = queue.shift()) await drawOne(t)
  }
  await Promise.all(Array.from({ length: Math.min(DRAW_CONCURRENCY, queue.length) }, worker))
  if (request.signal.aborted) return refuse(499, "The export was cancelled")

  const generatedAt = new Date()
  let pdf: Buffer
  try {
    pdf = await dashboardPdf({
      name: dashboard.name,
      filters: filterLines(filters, sent),
      generatedAt,
      tiles: tiles.map((t) => {
        const got = draws.get(t.id)
        return pdfTileOf(t, got?.draw ?? null, got?.failure ?? null, filters)
      }),
    })
  } catch (err) {
    console.error("Dashboard PDF: building the PDF failed:", err)
    return refuse(500, "Could not build the PDF")
  }

  const name = exportFilename(dashboard.name, "pdf", "dashboard", generatedAt)
  // RFC 5987: encodeURIComponent leaves ' ( ) * as they are, and ' ends the charset part.
  const utf8 = encodeURIComponent(name.utf8).replace(/['()*]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)
  return new NextResponse(new Uint8Array(pdf), {
    status: 200,
    headers: {
      "Content-Type": "application/pdf",
      "Content-Disposition": `attachment; filename="${name.ascii}"; filename*=UTF-8''${utf8}`,
      "Content-Length": String(pdf.length),
      "Cache-Control": "no-store",
    },
  })
}
