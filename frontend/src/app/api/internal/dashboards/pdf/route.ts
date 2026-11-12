// A workflow's dashboard as a PDF for its email (dashboards-design.md § 8, D3). The
// gateway's dashboard step (workflow_dashboard_step.go frontendDashboardPDF) has already
// drawn every tile as the run's user; it posts the board's name, its tiles as
// GET /api/v1/dashboards/:id gives them and each chart tile's answer, and gets back the
// PDF the board's Export PDF button makes (lib/export/dashboard-pdf.ts). Nothing here
// reads data or a session, so only a caller holding INTERNAL_SERVICE_SECRET may ask.
// Traefik sends /api/* to the gateway, so outside callers reach the gateway, not this route.
import { createHash, timingSafeEqual } from "node:crypto"

import { NextResponse } from "next/server"

import { asDraw, type ChartDraw } from "@/lib/api/charts"
import type { DashboardTile, TileDraw } from "@/lib/api/dashboards"
import { dashboardPdf, pdfTileOf } from "@/lib/export/dashboard-pdf"

export const dynamic = "force-dynamic"
export const runtime = "nodejs"

const refuse = (status: number, error: string) => NextResponse.json({ error }, { status })

const digest = (s: string) => createHash("sha256").update(s).digest()

/** The caller's secret against ours, in a time that says nothing about how much matched. */
function secretMatches(given: string | null, secret: string): boolean {
  return given !== null && timingSafeEqual(digest(given), digest(secret))
}

const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v)

const isTile = (t: unknown): t is DashboardTile =>
  isObject(t) &&
  typeof t.id === "string" &&
  (t.kind === "chart" || t.kind === "text") &&
  ["x", "y", "w", "h"].every((k) => typeof t[k] === "number")

/** A tile's answer as the gateway posts it (dashboardPDFTileDraw): a draw, or why there is none. */
type PostedDraw = { draw: TileDraw | null; failure: string | null }

function readDraw(v: unknown): PostedDraw | string {
  if (!isObject(v)) return "each draw must be an object"
  if (v.failure !== undefined && v.failure !== null) {
    if (typeof v.failure !== "string") return "a draw's failure must be a string"
    return { draw: null, failure: v.failure }
  }
  if (!isObject(v.draw)) return "a draw needs a draw or a failure"
  const d = v.draw as Partial<TileDraw>
  const r = d.result as unknown
  if (r !== undefined && r !== null) {
    if (!isObject(r) || !Array.isArray(r.columns) || !Array.isArray(r.rows) || !r.rows.every(Array.isArray)) {
      return "a draw's result must have columns and rows"
    }
  }
  const base: ChartDraw = asDraw(d)
  return {
    draw: {
      ...base,
      // The gateway leaves out the SQL and timings, which the PDF does not show.
      result: base.result ? { ...base.result, sql: "", read_at: "", execution_time_ms: 0 } : null,
      chart: d.chart ?? null,
      filters: { applied: [], ignored: [], unfiltered: [] },
    },
    failure: null,
  }
}

/** The posted board, or why it is not one. */
function readRequest(body: unknown): { name: string; generatedAt: Date; tiles: DashboardTile[]; draws: Map<string, PostedDraw> } | string {
  if (!isObject(body)) return "The body must be a JSON object"
  const { name, generated_at: generatedAt, tiles, draws } = body
  if (typeof name !== "string") return "name must be a string"
  const at = typeof generatedAt === "string" ? new Date(generatedAt) : null
  if (!at || Number.isNaN(at.getTime())) return "generated_at must be a time"
  if (!Array.isArray(tiles) || !tiles.every(isTile)) return "tiles must be the dashboard's tiles"
  if (!isObject(draws)) return "draws must be an object"
  const out = new Map<string, PostedDraw>()
  for (const [id, v] of Object.entries(draws)) {
    const d = readDraw(v)
    if (typeof d === "string") return `draws.${id}: ${d}`
    out.set(id, d)
  }
  return { name, generatedAt: at, tiles, draws: out }
}

export async function POST(request: Request) {
  const secret = process.env.INTERNAL_SERVICE_SECRET?.trim()
  if (!secret) return refuse(503, "INTERNAL_SERVICE_SECRET is not set, so this route answers no one")
  if (!secretMatches(request.headers.get("x-internal-secret"), secret)) return refuse(401, "Not an internal caller")

  let body: unknown
  try {
    body = await request.json()
  } catch {
    return refuse(400, "The request body must be JSON")
  }
  const board = readRequest(body)
  if (typeof board === "string") return refuse(400, board)

  let pdf: Buffer
  try {
    pdf = await dashboardPdf({
      name: board.name,
      // A workflow draws the board with no filters picked.
      filters: [],
      generatedAt: board.generatedAt,
      tiles: board.tiles.map((t) => {
        const got = board.draws.get(t.id)
        return pdfTileOf(t, got?.draw ?? null, got?.failure ?? null, [])
      }),
    })
  } catch (err) {
    console.error("Workflow dashboard PDF: building the PDF failed:", err)
    return refuse(500, "The PDF could not be built")
  }
  return new Response(new Uint8Array(pdf), {
    status: 200,
    headers: { "Content-Type": "application/pdf", "Content-Length": String(pdf.length), "Cache-Control": "no-store" },
  })
}
