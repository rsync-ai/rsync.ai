// A workflow's chart as a PNG for its email (dashboards-design.md § 8, D3). The
// gateway's chart step (workflow_chart_step.go frontendChartPNG) has already read the
// chart's rows as the run's user; it posts the chart's name, settings and those rows,
// and gets back the picture lib/export/email-chart.ts draws. Nothing here reads data or
// a session, so only a caller holding INTERNAL_SERVICE_SECRET may ask. Traefik sends
// /api/* to the gateway, so outside callers reach the gateway, not this route.
import { createHash, timingSafeEqual } from "node:crypto"

import { NextResponse } from "next/server"

import { CHART_TYPES, type ChartOutput, type ChartResult, type ServerChartSettings } from "@/lib/api/charts"
import { emailChartPng, NotPlotted } from "@/lib/export/email-chart"

export const dynamic = "force-dynamic"
export const runtime = "nodejs"

const refuse = (status: number, error: string) => NextResponse.json({ error }, { status })

const digest = (s: string) => createHash("sha256").update(s).digest()

/** The caller's secret against ours, in a time that says nothing about how much matched. */
function secretMatches(given: string | null, secret: string): boolean {
  return given !== null && timingSafeEqual(digest(given), digest(secret))
}

const isStrings = (v: unknown): v is string[] => Array.isArray(v) && v.every((s) => typeof s === "string")

const isColumn = (c: unknown): c is ChartOutput =>
  !!c && typeof c === "object" && typeof (c as ChartOutput).alias === "string" && typeof (c as ChartOutput).role === "string"

/** The posted chart, or why it is not one. */
function readRequest(body: unknown): { name: string; settings: ServerChartSettings; result: ChartResult } | string {
  if (!body || typeof body !== "object") return "The body must be a JSON object"
  const { name, settings, result } = body as Record<string, unknown>
  if (typeof name !== "string") return "name must be a string"
  const st = settings as Partial<ServerChartSettings> | null
  if (!st || typeof st !== "object" || !CHART_TYPES.includes(st.type!)) return "settings must be a chart's settings"
  // The gateway hands every chart on at version 2 (charts.AsVersion2), its fields by role.
  if (st.version !== 2 || !st.roles || typeof st.roles !== "object") return "settings must be version 2, by role"
  if (!result || typeof result !== "object") return "result must be an object"
  const r = result as Record<string, unknown>
  if (!Array.isArray(r.columns) || !r.columns.every(isColumn)) return "result.columns must be the chart's columns"
  if (!Array.isArray(r.rows) || !r.rows.every(Array.isArray)) return "result.rows must be a list of rows"
  if (typeof r.truncated !== "boolean" || !isStrings(r.masked) || !isStrings(r.unfiltered)) {
    return "result needs truncated, masked and unfiltered"
  }
  return {
    name,
    settings: settings as ServerChartSettings,
    result: {
      columns: r.columns,
      rows: r.rows as unknown[][],
      truncated: r.truncated,
      masked: r.masked,
      unfiltered: r.unfiltered,
      sql: "",
      read_at: "",
      execution_time_ms: 0,
    },
  }
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
  const chart = readRequest(body)
  if (typeof chart === "string") return refuse(400, chart)

  let png: Buffer
  try {
    png = emailChartPng(chart.name, chart.settings, chart.result)
  } catch (err) {
    if (err instanceof NotPlotted) return refuse(422, err.message)
    console.error("Workflow chart PNG: drawing failed:", err)
    return refuse(500, "The chart could not be drawn")
  }
  return new Response(new Uint8Array(png), {
    status: 200,
    headers: { "Content-Type": "image/png", "Cache-Control": "no-store" },
  })
}
