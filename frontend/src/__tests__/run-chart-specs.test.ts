import { readFileSync } from "node:fs"
import { relative } from "node:path"
import { describe, expect, it } from "vitest"

import { chartTable } from "@/components/charts/chart-option"
import type { XYSettings } from "@/components/charts/chart-settings"
import { CHART_COLORS } from "@/components/charts/chart-theme"
import {
  CHECK_OUTCOMES,
  MODEL_OUTCOMES,
  PIPELINE_OUTCOMES,
  RUN_TIME_MEASURES,
  countChart,
  countRows,
  dayLabel,
  recentRunsChart,
  rowsWrittenChart,
  rowsWrittenRows,
  runTimeChart,
  runTimeRows,
  runOutcome,
  type Outcome,
} from "@/components/runs/run-chart-specs"
import { SRC, sourceFiles } from "./class-literals"

// The Runs page's charts (dashboards-design.md § 6, D1). These pin that the chart
// settings are plain JSON naming colour tokens and never a colour, that each outcome
// keeps its own token in the server's order, that every row the page builds lands
// in the chart, and that ECharts is reachable only through the lazily loaded chart.

const DAYS = ["2026-10-03", "2026-10-04", "2026-10-05"]

const COUNT_CHARTS: [string, readonly Outcome[]][] = [
  ["pipeline", PIPELINE_OUTCOMES],
  ["model", MODEL_OUTCOMES],
  ["check", CHECK_OUTCOMES],
]

function allCharts(): [string, XYSettings][] {
  return [
    ...COUNT_CHARTS.map(([name, o]) => [`${name} counts`, countChart(name, DAYS, o)] as [string, XYSettings]),
    ["run time", runTimeChart("run time", DAYS)],
    ["rows written", rowsWrittenChart("rows written", DAYS)],
  ]
}

describe("day labels", () => {
  it("say a UTC day as the axis does, whatever the browser's zone", () => {
    expect(dayLabel("2026-10-03")).toBe("Oct 3")
    expect(dayLabel("2026-01-01")).toBe("Jan 1")
    expect(dayLabel("2026-12-31")).toBe("Dec 31")
    expect(dayLabel("garbage")).toBe("garbage")
  })
})

describe("chart rows", () => {
  it("label each count with its outcome", () => {
    const rows = countRows(
      [
        { day: "2026-10-03", status: "succeeded", count: 4 },
        { day: "2026-10-03", status: "running", count: 1 },
        { day: "2026-10-04", status: "failed", count: 2 },
      ],
      PIPELINE_OUTCOMES,
    )
    expect(rows).toEqual([
      { day: "Oct 3", outcome: "Succeeded", count: 4 },
      { day: "Oct 3", outcome: "Running", count: 1 },
      { day: "Oct 4", outcome: "Failed", count: 2 },
    ])
  })

  it("say check results as the Checks tab does", () => {
    const rows = countRows(
      [
        { day: "2026-10-03", status: "pass", count: 2 },
        { day: "2026-10-03", status: "fail", count: 1 },
        { day: "2026-10-03", status: "error", count: 1 },
      ],
      CHECK_OUTCOMES,
    )
    expect(rows.map((r) => r.outcome)).toEqual(["Passed", "Failed", "Could not run"])
  })

  it("give every outcome the server sends a place (activity_metrics.go *MetricStatusOrder)", () => {
    const go = (f: string) => readFileSync(`${SRC}/../../api-gateway/internal/handlers/${f}`, "utf8")
    const src = go("activity_metrics.go")
    const consts = Object.fromEntries(
      [...(go("activity.go") + src).matchAll(/(\w+)\s*=\s*"(\w+)"/g)].map((m) => [m[1], m[2]]),
    )
    const order = (name: string) => {
      const m = src.match(new RegExp(`${name}\\s*=\\s*\\[\\]string\\{([^}]*)\\}`))
      expect(m, name).not.toBeNull()
      return m![1].split(",").map((s) => consts[s.trim()] ?? s.trim())
    }
    expect(PIPELINE_OUTCOMES.map((o) => o.status)).toEqual(order("pipelineMetricStatusOrder"))
    expect(MODEL_OUTCOMES.map((o) => o.status)).toEqual(order("modelMetricStatusOrder"))
    expect(CHECK_OUTCOMES.map((o) => o.status)).toEqual(order("checkMetricStatusOrder"))
  })

  it("colour an outcome by what it means, the same on every chart", () => {
    const pairs = (o: readonly Outcome[]) => o.map((x) => `${x.status}:${x.color}`)
    expect(pairs(PIPELINE_OUTCOMES)).toEqual([
      "succeeded:succeeded",
      "failed:failed",
      "cancelled:cancelled",
      "waiting:waiting",
      "running:running",
    ])
    expect(pairs(MODEL_OUTCOMES)).toEqual(["succeeded:succeeded", "failed:failed", "skipped:skipped"])
    expect(pairs(CHECK_OUTCOMES)).toEqual(["pass:succeeded", "fail:failed", "error:error"])
  })

  it("break the run-time line on a day nothing finished", () => {
    const rows = runTimeRows(DAYS, [{ day: "2026-10-04", runs: 2, median_seconds: 40, p95_seconds: 58 }])
    expect(rows).toEqual([
      { day: "Oct 3", measure: "Median", seconds: null, runs: 0 },
      { day: "Oct 3", measure: "95th percentile", seconds: null, runs: 0 },
      { day: "Oct 4", measure: "Median", seconds: 40, runs: 2 },
      { day: "Oct 4", measure: "95th percentile", seconds: 58, runs: 2 },
      { day: "Oct 5", measure: "Median", seconds: null, runs: 0 },
      { day: "Oct 5", measure: "95th percentile", seconds: null, runs: 0 },
    ])
  })

  it("keep rows written as the server counted them", () => {
    expect(rowsWrittenRows([{ day: "2026-10-03", rows: 105 }])).toEqual([{ day: "Oct 3", rows: 105 }])
  })

  it("drop a status the chart does not have", () => {
    expect(countRows([{ day: "2026-10-03", status: "mystery", count: 9 }], PIPELINE_OUTCOMES)).toEqual([])
  })
})

describe("chart settings", () => {
  it.each(allCharts())("%s is plain JSON", (_, settings) => {
    expect(JSON.parse(JSON.stringify(settings))).toEqual(settings)
  })

  it.each(allCharts())("%s names colour tokens, never a colour", (_, settings) => {
    expect(JSON.stringify(settings)).not.toMatch(/#[0-9a-f]{3,8}\b|hsla?\(|rgba?\(/i)
    for (const s of settings.series.values) expect(CHART_COLORS).toContain(s.color)
  })

  it.each(allCharts())("%s puts every day of the window on the axis", (_, settings) => {
    expect(settings.x.categories).toEqual(["Oct 3", "Oct 4", "Oct 5"])
  })

  it.each(COUNT_CHARTS)("%s counts stack each outcome in its own token, in order", (name, outcomes) => {
    const settings = countChart(name, DAYS, outcomes)
    expect(settings).toMatchObject({ type: "bar", stacked: true })
    expect(settings.series.values).toEqual(outcomes.map((o) => ({ key: o.label, label: o.label, color: o.color })))
  })

  it("median and 95th percentile take the first two series colours", () => {
    const settings = runTimeChart("t", DAYS)
    expect(settings.series.values.map((s) => s.key)).toEqual([...RUN_TIME_MEASURES])
    expect(settings.series.values.map((s) => s.color)).toEqual(["series-1", "series-2"])
  })
})

describe("every row lands in its chart", () => {
  // A row whose field names or labels drift from the settings' is silently not drawn.
  const filled = (settings: XYSettings, rows: object[]) =>
    chartTable(settings, rows).body.flatMap((cells) => cells.slice(1, 1 + settings.series.values.length)).filter(Boolean)

  it.each(COUNT_CHARTS)("%s counts", (name, outcomes) => {
    const rows = countRows(
      DAYS.flatMap((day) => outcomes.map((o) => ({ day, status: o.status, count: 1 }))),
      outcomes,
    )
    expect(filled(countChart(name, DAYS, outcomes), rows)).toHaveLength(DAYS.length * outcomes.length)
  })

  it("run time", () => {
    const rows = runTimeRows(DAYS, [{ day: "2026-10-04", runs: 2, median_seconds: 40, p95_seconds: 58 }])
    expect(filled(runTimeChart("t", DAYS), rows)).toEqual(["40", "58"])
    // Finished runs, from the details.
    expect(chartTable(runTimeChart("t", DAYS), rows).body.map((cells) => cells.at(-1))).toEqual(["0", "2", "0"])
  })

  it("rows written", () => {
    const rows = rowsWrittenRows(DAYS.map((day) => ({ day, rows: 1000 })))
    expect(filled(rowsWrittenChart("r", DAYS), rows)).toHaveLength(DAYS.length)
  })
})

// Run detail's "Last 10 runs" and the pipeline Overview's recent runs.
describe("a pipeline's recent runs", () => {
  it("map each stored status to its outcome the way the gateway does (activity.go pipelineRunStatuses)", () => {
    const outcome = (s: string | undefined) => runOutcome(s).status
    expect(["completed", "success", "SUCCESS"].map(outcome)).toEqual(["succeeded", "succeeded", "succeeded"])
    expect(["failed", "error", "silent_drop_detected", "silent_partial_drop_detected", "credential_check_failed"].map(outcome)).toEqual(
      Array(5).fill("failed"),
    )
    expect(outcome("cancelled")).toBe("cancelled")
    expect(outcome("waiting_for_user")).toBe("waiting")
    // Not finished: running, pending, a progress phase, nothing at all.
    expect(["running", "pending", "extracting", undefined].map(outcome)).toEqual(Array(4).fill("running"))
  })

  const NOW = Date.parse("2026-10-07T12:00:00Z")
  const RUNS = [
    { execution_id: "a", status: "completed", start_time: "2026-10-07T09:00:00Z", duration_ms: 18_400 },
    { execution_id: "b", status: "failed", start_time: "2026-10-07T09:00:30Z", duration_ms: 4_200 },
    { execution_id: "c", status: "cancelled", start_time: "2026-10-07T10:00:00Z" },
    { execution_id: "d", status: "running", start_time: "2026-10-07T11:59:00Z" },
  ]

  it("draws one bar per run, oldest first, as long as it took, in its outcome's colour", () => {
    const { settings, rows } = recentRunsChart("Last runs", RUNS, { now: NOW })
    expect(settings.type).toBe("bar")
    expect(settings.x.categories).toHaveLength(4)
    // Two runs in the same minute stay two bars.
    expect(new Set(settings.x.categories).size).toBe(4)
    expect(rows.map((r) => [r.outcome, r.seconds])).toEqual([
      ["Succeeded", 18.4],
      ["Failed", 4.2],
      // A cancelled run has no end, so no height; a running one is as long as it has run.
      ["Cancelled", null],
      ["Running", 60],
    ])
    expect(rows.map((r) => r.run)).toEqual(settings.x.categories)
  })

  it("outlines the run the page is about, and none that is not in the list", () => {
    const { settings } = recentRunsChart("Last runs", RUNS, { now: NOW, highlight: "b" })
    expect(settings.type === "bar" && settings.highlight).toBe(settings.x.categories[1])
    const none = recentRunsChart("Last runs", RUNS, { now: NOW, highlight: "zzz" }).settings
    expect(none.type === "bar" && none.highlight).toBeUndefined()
    // A cancelled run has no bar to outline, so the card must not say one is.
    const barless = recentRunsChart("Last runs", RUNS, { now: NOW, highlight: "c" }).settings
    expect(barless.type === "bar" && barless.highlight).toBeUndefined()
  })

  it("lands every run in its chart", () => {
    const { settings, rows } = recentRunsChart("Last runs", RUNS, { now: NOW })
    const body = chartTable(settings, rows).body
    expect(body.map((cells) => cells.slice(1, 1 + settings.series.values.length).filter(Boolean))).toEqual([["18.4"], ["4.2"], [], ["60"]])
  })
})

describe("ECharts loads only with the lazy chart", () => {
  const files = sourceFiles(SRC).map((f) => ({ path: relative(SRC, f), src: readFileSync(f, "utf8") }))
  // An import's names hold no quote, so [^"']* stays inside one statement.
  const PKG = `(echarts|zrender)(/[^"']*)?`
  const ECHARTS_RUNTIME = new RegExp(
    `^\\s*import\\s+(?!type\\b)[^"']*?from\\s+["']${PKG}["']|^\\s*import\\s+["']${PKG}["']|\\b(import|require)\\(\\s*["']${PKG}["']`,
    "m",
  )
  const ECHART = /["']@\/components\/charts\/EChart["']/

  it("the check can fail (control)", () => {
    expect(ECHARTS_RUNTIME.test(`import { init } from "echarts/core"`)).toBe(true)
    expect(ECHARTS_RUNTIME.test(`import "zrender"`)).toBe(true)
    expect(ECHARTS_RUNTIME.test(`const m = await import("echarts")`)).toBe(true)
    expect(ECHARTS_RUNTIME.test(`import type { ComposeOption } from "echarts/core"`)).toBe(false)
  })

  it("only EChart.tsx, its lazy modules and the export's two drawers import ECharts at run time", () => {
    expect(files.filter((f) => ECHARTS_RUNTIME.test(f.src)).map((f) => f.path).sort()).toEqual([
      "components/charts/EChart.tsx",
      "components/charts/echart-calendar.ts",
      "components/charts/echart-flows.ts",
      "components/charts/echart-maps.ts",
      "components/charts/echart-parts.ts",
      "components/charts/echart-points.ts",
      "components/charts/echart-shapes.ts",
      "components/charts/echart-stats.ts",
      "lib/export/chart-image.ts",
      "lib/export/chart-svg.ts",
    ])
  })

  // A type's own modules (a pie's, a scatter's, a treemap's, a sankey's, a box plot's, a
  // calendar's, a map's…) load with the first such chart (§ 14.6), so a page that draws
  // only bars and lines never fetches them. A chart's image loads the same group, not a
  // copy. The server's drawer imports the map group, which holds the shapes it registers.
  it("a type's own ECharts modules load only through import(), from the chart", () => {
    const STATIC = /^\s*import\s[^"']*["']@\/components\/charts\/echart-(shapes|points|parts|flows|stats|calendar|maps)["']/m
    const LAZY = (group: string) => new RegExp(`\\bimport\\(\\s*["']@/components/charts/echart-${group}["']\\s*\\)`)
    expect(files.filter((f) => STATIC.test(f.src)).map((f) => f.path)).toEqual(["lib/export/chart-svg.ts"])
    for (const group of ["shapes", "points", "parts", "flows", "stats", "calendar", "maps"])
      expect(files.filter((f) => LAZY(group).test(f.src)).map((f) => f.path)).toEqual(["components/charts/chart-modules.ts"])
    // The shapes come only with the map group: a page reads a row's code (map-regions.ts), never a shape.
    const SHAPES = /^\s*import\s[^"']*["'](world-atlas[^"']*|topojson-client|@\/components\/charts\/map-shapes\/us-states\.json)["']|\bimport\(\s*["'](world-atlas|topojson-client)/m
    expect(files.filter((f) => SHAPES.test(f.src)).map((f) => f.path)).toEqual(["components/charts/echart-maps.ts"])
    const users = files.filter((f) => f.src.includes(`"@/components/charts/chart-modules"`)).map((f) => f.path)
    expect(users).toEqual(["components/charts/EChart.tsx", "lib/export/chart-image.ts"])
  })

  // A chart's image is drawn in the browser only when one is asked for; the one static
  // user is the workflow email's drawer, which runs on the server (below).
  it("the image drawer is reached only through import()", () => {
    const STATIC = /^\s*import\s[^"']*["']@\/lib\/export\/chart-image["']/m
    const LAZY = /\bimport\(\s*["']@\/lib\/export\/chart-image["']\s*\)/
    expect(files.filter((f) => STATIC.test(f.src)).map((f) => f.path)).toEqual(["lib/export/email-chart.ts"])
    expect(files.filter((f) => LAZY.test(f.src)).map((f) => f.path)).toEqual(["components/charts/ChartExportMenu.tsx"])
  })

  // A dashboard's PDF and a workflow email's chart are drawn on the server: no page
  // reaches either drawer.
  it("the server drawers are reached only from their routes", () => {
    const users = (mod: string) => files.filter((f) => f.src.includes(`"@/lib/export/${mod}"`)).map((f) => f.path).sort()
    expect(users("chart-svg")).toEqual(["lib/export/dashboard-pdf.ts", "lib/export/email-chart.ts"])
    expect(users("dashboard-pdf")).toEqual([
      "app/api/internal/dashboards/pdf/route.ts",
      "app/proxy/dashboards/[id]/pdf/route.ts",
      "lib/export/email-chart.ts",
    ])
    expect(users("email-chart")).toEqual(["app/api/internal/charts/png/route.ts"])
    for (const path of [
      "lib/export/chart-svg.ts",
      "lib/export/dashboard-pdf.ts",
      "lib/export/email-chart.ts",
      "app/proxy/dashboards/[id]/pdf/route.ts",
      "app/api/internal/charts/png/route.ts",
      "app/api/internal/dashboards/pdf/route.ts",
    ]) {
      expect(files.find((f) => f.path === path)?.src).not.toMatch(/^\s*["']use client["']/m)
    }
  })

  it("EChart is reached only through next/dynamic with ssr: false", () => {
    const users = files.filter((f) => ECHART.test(f.src))
    expect(users.map((f) => f.path)).toEqual(["components/charts/EChartNoSSR.tsx"])
    const src = users[0].src
    expect(src).toMatch(/dynamic\(\s*\(\)\s*=>\s*import\("@\/components\/charts\/EChart"\)/)
    expect(src).toMatch(/ssr:\s*false/)
    expect(src).not.toMatch(/^\s*import\s[^"']*["']@\/components\/charts\/EChart["']/m)
  })

  it("ECharts is pinned to one version", () => {
    const pkg = JSON.parse(readFileSync(`${SRC}/../package.json`, "utf8"))
    expect(pkg.dependencies.echarts).toMatch(/^\d+\.\d+\.\d+$/)
  })
})
