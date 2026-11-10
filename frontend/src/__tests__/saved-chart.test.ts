import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { describe, expect, it } from "vitest"

import {
  EMPTY_FORM,
  TYPE_GROUPS,
  fromSettings,
  measureColumns,
  missingField,
  refusedTypes,
  suggestedTypes,
  takesX,
  toSettings,
  wrongColumn,
  type ChartForm,
} from "@/components/charts/chart-form"
import type { ChartAggregate, ChartType } from "@/lib/api/charts"
import { CHART_REGISTRY } from "@/components/charts/chart-registry"
import { toCsv } from "@/lib/export/csv"
import {
  BOX_ROLES,
  MAX_SANKEY_NODES,
  PRICE_ROLES,
  ROLE_OTHER,
  ROLE_POINT_ROWS,
  ROLE_SECOND_MEASURE,
  ROLE_SERIES_COUNT,
  ROLE_SIZE,
  assignSeriesColors,
  changeShort,
  changeText,
  grainLabel,
  labelOf,
  levelRole,
  measureFormat,
  measureTitle,
  otherNote,
  pivotRole,
  pivotTotalRole,
  savedChartView,
  tableExport,
  toNumber,
  truncatedNote,
} from "@/components/charts/saved-chart"
import { binLabel, formatNumber, type NumberFormat } from "@/components/charts/chart-settings"
import { one, type ChartField, type ChartGrain, type ChartResult, type ServerChartSettings } from "@/lib/api/charts"
import { SRC } from "./class-literals"

// The chart page's fields and a saved chart's view (dashboards-design.md § 3, § 7.2).
// The page sends a type only the fields it draws, the shape charts/settings.go holds it
// to; a drawn result is read by the role the server names each column, never by a guess.
// Settings are version 2, each field under the role it fills (§ 14.2).

const form = (f: Partial<ChartForm>): ChartForm => ({ ...EMPTY_FORM, ...f })
const v2 = (type: ServerChartSettings["type"], roles: ServerChartSettings["roles"], rest: Partial<ServerChartSettings> = {}): ServerChartSettings => ({
  version: 2,
  type,
  roles,
  ...rest,
})

describe("toSettings", () => {
  it("never stacks a line, and stacks bars and areas only with a series", () => {
    const base = { x: "day", aggregate: "count" as const, series: "region", stacked: true }
    expect(toSettings(form({ ...base, type: "line" })).stacked).toBeUndefined()
    expect(toSettings(form({ ...base, type: "bar" })).stacked).toBe(true)
    expect(toSettings(form({ ...base, type: "hbar" })).stacked).toBe(true)
    expect(toSettings(form({ ...base, type: "area" })).stacked).toBe(true)
    expect(toSettings(form({ ...base, type: "bar", series: "" })).stacked).toBeUndefined()
  })

  it("sends a scorecard no x, no series and no sort left over from another type", () => {
    expect(
      toSettings(
        form({
          type: "scorecard",
          x: "day",
          series: "region",
          sort: "x_asc",
          stacked: true,
          aggregate: "sum",
          measureColumn: "amount",
          compareColumn: "day",
          compareDays: 30,
        }),
      ),
    ).toEqual(v2("scorecard", { measure: [{ aggregate: "sum", column: "amount" }], date: [{ column: "day" }] }, { compare: { days: 30 } }))
  })

  it("sends a count no column", () => {
    expect(toSettings(form({ x: "day", aggregate: "count", measureColumn: "amount" })).roles.measure).toEqual([{ aggregate: "count" }])
  })

  it("sends a time x its grain, and a heatmap's y its own", () => {
    expect(toSettings(form({ type: "line", x: "day", xGrain: "month", aggregate: "count" })).roles.x).toEqual([{ column: "day", grain: "month" }])
    const heat = toSettings(form({ type: "heatmap", x: "day", xGrain: "week", y: "shipped", yGrain: "quarter", aggregate: "count" }))
    expect(heat.roles.x).toEqual([{ column: "day", grain: "week" }])
    expect(heat.roles.y).toEqual([{ column: "shipped", grain: "quarter" }])
    // No grain picked: x groups by each value, and no grain is sent.
    expect(toSettings(form({ x: "day", aggregate: "count" })).roles.x).toEqual([{ column: "day" }])
  })

  it("pins at most 7 series colours, and the measure's colour only without a series", () => {
    const pins = Array.from({ length: 9 }, (_, i) => ({ value: `v${i}`, color: `series-${(i % 7) + 1}` }))
    const withSeries = toSettings(form({ x: "day", aggregate: "count", series: "region", color: "series-3" }), pins)
    expect(withSeries.roles.series).toEqual([{ column: "region", colors: pins.slice(0, 7) }])
    expect(withSeries.roles.measure).toEqual([{ aggregate: "count" }])
    expect(toSettings(form({ x: "day", aggregate: "count", series: "region" }), []).roles.series).toEqual([{ column: "region" }])
    expect(toSettings(form({ x: "day", aggregate: "count", color: "series-3" })).roles.measure).toEqual([
      { aggregate: "count", color: "series-3" },
    ])
  })

  it("sends a heatmap its y and colour, no series, and a sort by x only", () => {
    const heat = form({ type: "heatmap", x: "day", y: "region", aggregate: "count", color: "series-4", series: "s" })
    expect(toSettings({ ...heat, sort: "value_desc" })).toEqual(
      v2("heatmap", { x: [{ column: "day" }], y: [{ column: "region" }], measure: [{ aggregate: "count", color: "series-4" }] }),
    )
    expect(toSettings({ ...heat, sort: "x_desc" }).sort).toBe("x_desc")
  })

  it("sends a table its first 20 columns, no x, and a sort by value only with a measure", () => {
    const columns = Array.from({ length: 25 }, (_, i) => `c${i}`)
    const table = toSettings(form({ type: "table", x: "day", columns, sort: "value_desc" }))
    expect(table).toEqual(v2("table", { columns: columns.slice(0, 20).map((column) => ({ column })) }))
    expect(toSettings(form({ type: "table", columns: ["a"], sort: "x_asc" })).sort).toBe("x_asc")
    expect(
      toSettings(form({ type: "table", columns: ["a"], aggregate: "max", measureColumn: "b", sort: "value_desc" })).sort,
    ).toBe("value_desc")
  })

  it("trims text and leaves out what is blank", () => {
    expect(
      toSettings(form({ description: "  ", x: "day", xTitle: "  ", aggregate: "sum", measureColumn: "a", measureTitle: " Revenue " })),
    ).toEqual(v2("bar", { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "a", title: "Revenue" }] }))
    // No aggregate picked: no measure at all, never a default.
    expect(toSettings(form({ x: "day" })).roles.measure).toBeUndefined()
  })
})

describe("fromSettings", () => {
  // Each type as the server accepts it (charts/settings_test.go TestDecodeAcceptsEachType).
  const saved: ServerChartSettings[] = [
    v2(
      "bar",
      {
        x: [{ column: "day", title: "Day", grain: "month" }],
        measure: [{ aggregate: "sum", column: "amount", title: "Revenue", format: "compact" }],
        series: [{ column: "region", colors: [{ value: "EU", color: "series-2" }] }],
      },
      { description: "Daily revenue", stacked: true, sort: "x_asc" },
    ),
    v2("hbar", { x: [{ column: "region" }], measure: [{ aggregate: "count_distinct", column: "customer_id" }] }, { sort: "value_desc" }),
    v2("line", { x: [{ column: "day", grain: "week" }], measure: [{ aggregate: "avg", column: "amount", color: "series-3" }] }),
    v2("area", { x: [{ column: "day" }], measure: [{ aggregate: "max", column: "amount" }], series: [{ column: "region" }] }, { stacked: true }),
    v2(
      "heatmap",
      { x: [{ column: "day", grain: "year" }], y: [{ column: "shipped", grain: "quarter" }], measure: [{ aggregate: "count", color: "series-4" }] },
      { sort: "x_desc" },
    ),
    v2("scorecard", { measure: [{ aggregate: "sum", column: "amount" }], date: [{ column: "day" }] }, { compare: { days: 30 } }),
    v2("table", { columns: [{ column: "day" }, { column: "region" }], measure: [{ aggregate: "min", column: "amount" }] }, { sort: "value_desc" }),
    // D4b (§ 14.9).
    v2(
      "bar",
      {
        x: [{ column: "day", grain: "month" }],
        measure: [
          { aggregate: "count", mark: "bar", color: "series-2" },
          { aggregate: "sum", column: "amount", title: "Revenue", format: "compact", mark: "line", axis: "right" },
        ],
      },
      { sort: "x_asc" },
    ),
    v2("area", { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount" }], series: [{ column: "region" }] }, { stacked: true, percent: true }),
    v2("pie", { series: [{ column: "plan", colors: [{ value: "Pro", color: "series-3" }] }], measure: [{ aggregate: "sum", column: "amount" }] }),
    v2("donut", { series: [{ column: "plan" }], measure: [{ aggregate: "count" }] }),
    v2("waterfall", { x: [{ column: "day", grain: "quarter" }], measure: [{ aggregate: "sum", column: "net" }] }, { sort: "x_asc" }),
    v2("funnel", { x: [{ column: "stage", title: "Stage" }], measure: [{ aggregate: "count_distinct", column: "user_id", color: "series-5" }] }, { sort: "value_desc" }),
    v2("histogram", { x: [{ column: "amount", bins: 25 }] }),
    v2("histogram", { x: [{ column: "amount", title: "Order size", width: 2.5 }] }),
    v2("scorecard", { measure: [{ aggregate: "count" }], date: [{ column: "day" }] }, { compare: { days: 30 }, sparkline: true }),
  ]

  it.each(saved.map((s) => [s.type, s] as const))("fills the fields a saved %s chart made", (_, s) => {
    expect(toSettings(fromSettings(s), one(s, "series")?.colors)).toEqual(s)
  })
})

describe("missingField", () => {
  it("says what the type still needs, in order", () => {
    expect(missingField(form({ type: "bar" }))).toBe("Pick the x column.")
    expect(missingField(form({ type: "line", x: "day" }))).toBe("Pick what to measure.")
    expect(missingField(form({ type: "area", x: "day", aggregate: "sum" }))).toBe("Pick the column to measure.")
    expect(missingField(form({ type: "hbar", x: "day", aggregate: "count" }))).toBeNull()
    expect(missingField(form({ type: "heatmap", x: "day" }))).toBe("Pick the y column.")
    expect(missingField(form({ type: "heatmap", x: "day", y: "r", aggregate: "avg", measureColumn: "a" }))).toBeNull()
    expect(missingField(form({ type: "scorecard", x: "day" }))).toBe("Pick what to measure.")
    expect(missingField(form({ type: "table" }))).toBe("Pick at least one column.")
    expect(missingField(form({ type: "table", columns: ["a"], aggregate: "sum" }))).toBe("Pick the column to measure.")
    // A table needs no measure.
    expect(missingField(form({ type: "table", columns: ["a"] }))).toBeNull()
  })

  it("holds the D4b types to what charts/settings.go accepts", () => {
    expect(missingField(form({ type: "pie" }))).toBe("Pick the column whose values are the slices.")
    expect(missingField(form({ type: "donut", series: "plan", aggregate: "avg", measureColumn: "a" }))).toBe(
      "A slice is a share of a whole: measure a count or a sum.",
    )
    expect(missingField(form({ type: "funnel" }))).toBe("Pick the column whose values are the stages.")
    expect(missingField(form({ type: "waterfall", x: "day", aggregate: "max", measureColumn: "a" }))).toBe(
      "Each step adds to a running total: measure a count or a sum.",
    )
    expect(missingField(form({ type: "histogram" }))).toBe("Pick the number column to count in bins.")
    expect(missingField(form({ type: "histogram", x: "amount" }))).toBeNull()
    // A combo: the second measure's column, two different measures, one on the left.
    const combo = { type: "bar" as const, x: "day", aggregate: "count" as const }
    expect(missingField(form({ ...combo, aggregate2: "sum" }))).toBe("Pick the column for the second measure.")
    expect(missingField(form({ ...combo, aggregate2: "count" }))).toBe(
      "The two measures are the same; measure another column or use another aggregate.",
    )
    expect(missingField(form({ ...combo, aggregate2: "sum", measureColumn2: "a", axis: "right", axis2: "right" }))).toBe(
      "Put one of the two measures on the left axis.",
    )
    // 100% adds the series up, so only a count or a sum; it counts only once it is sent.
    const shares = { type: "area" as const, x: "day", aggregate: "avg" as const, measureColumn: "a", series: "r", percent: true }
    expect(missingField(form({ ...shares, stacked: true }))).toBe("A share of 100% adds the series up: measure a count or a sum.")
    expect(missingField(form({ ...shares, stacked: false }))).toBeNull()
    expect(missingField(form({ ...shares, type: "line", stacked: true }))).toBeNull()
  })
})

// A pick kept from another type or aggregate, which the field's picker would not offer now.
describe("wrongColumn", () => {
  const COLUMNS = [
    { name: "day", kind: "time", masked: false },
    { name: "a", kind: "number", masked: false },
    { name: "b", kind: "number", masked: false },
    { name: "secret", kind: "number", masked: true },
    { name: "r", kind: "text", masked: false },
    { name: "ok", kind: "bool", masked: false },
    { name: "blob", kind: "unknown", masked: false },
  ]
  const wrong = (f: Partial<ChartForm>) => wrongColumn(form(f), COLUMNS)

  it("passes each column its field takes, and one the source no longer lists", () => {
    expect(wrong({ type: "bar", x: "r", aggregate: "sum", measureColumn: "a", series: "day" })).toBeNull()
    expect(wrong({ type: "scatter", x: "a", y: "b", series: "ok" })).toBeNull()
    expect(wrong({ type: "table", columns: ["r", "blob"], aggregate: "max", measureColumn: "day" })).toBeNull()
    expect(wrong({ type: "histogram", x: "gone" })).toBeNull()
    // A field the type does not send is never checked: a pie draws no x.
    expect(wrong({ type: "pie", x: "r", series: "r", aggregate: "count" })).toBeNull()
  })

  // The server's own rule (CheckKinds): a builder that refused more opened a saved chart
  // it could neither draw nor save again.
  it("passes a column the source read no kind for, as the server does", () => {
    // Its first value was null, say: the run says what it holds.
    expect(wrong({ type: "bubble", x: "a", y: "b", size: "blob" })).toBeNull()
    expect(wrong({ type: "bar", x: "r", aggregate: "sum", measureColumn: "blob" })).toBeNull()
    expect(wrong({ type: "scatter", x: "blob", y: "b", series: "blob" })).toBeNull()
  })

  it("passes a date held as a number, or as text where the chart reads it as a date, as the server does", () => {
    expect(wrong({ type: "calendar", x: "a", aggregate: "count" })).toBeNull()
    expect(wrong({ type: "calendar", x: "r", aggregate: "count" })).toBeNull()
    expect(wrong({ type: "candlestick", x: "a", prices: ["a", "b", "b", "a"] })).toBeNull()
    expect(wrong({ type: "candlestick", x: "r", xGrain: "month", prices: ["a", "b", "b", "a"] })).toBeNull()
    expect(wrong({ type: "scorecard", aggregate: "count", compareColumn: "a" })).toBeNull()
    // Ungrouped text would be drawn or compared as text.
    expect(wrong({ type: "candlestick", x: "r", prices: ["a", "b", "b", "a"] })).toBe("r is a text column. Pick the date column for the periods.")
    expect(wrong({ type: "scorecard", aggregate: "count", compareColumn: "r" })).toBe("r is a text column. Pick a date column to compare by, or none.")
  })

  it("names the column and what to pick, for each field that takes some kinds only", () => {
    expect(wrong({ type: "map", x: "day", aggregate: "count" })).toBe("day is a date column. Pick the column of country codes.")
    expect(wrong({ type: "bubble", x: "a", y: "b", size: "ok" })).toBe("ok is a yes-or-no column. Pick the number column each bubble's size reads.")
    expect(wrong({ type: "scatter", x: "a", y: "b", size: "r" })).toBe("r is a text column. Pick a number column for the size, or none.")
    expect(wrong({ type: "candlestick", x: "day", prices: ["a", "b", "r", "a"] })).toBe("r is a text column. Pick a number column for the low price.")
    expect(wrong({ type: "candlestick", x: "day", prices: ["r", "b", "a", "a"] })).toBe("r is a text column. Pick a number column for the open price.")
    expect(wrong({ type: "candlestick", x: "ok", prices: ["a", "b", "b", "a"] })).toBe("ok is a yes-or-no column. Pick the date column for the periods.")
    expect(wrong({ type: "calendar", x: "ok", aggregate: "count" })).toBe("ok is a yes-or-no column. Pick the date column for the days.")
    expect(wrong({ type: "scorecard", aggregate: "count", compareColumn: "ok" })).toBe(
      "ok is a yes-or-no column. Pick a date column to compare by, or none.",
    )
  })

  it("names a masked column wherever its values would be read", () => {
    expect(wrong({ type: "map", x: "secret", aggregate: "count" })).toBe("secret is masked, so its values are not read. Pick the column of country codes.")
    expect(wrong({ type: "boxplot", x: "r", y: "secret" })).toBe(
      "secret is masked, so its values are not read. Pick the number column to draw the boxes of.",
    )
    expect(wrong({ type: "bar", x: "r", aggregate: "min", measureColumn: "secret" })).toBe(
      "secret is masked, so its values are not read. Pick the column to measure.",
    )
  })

  it("names each measure a sum or an average cannot add up, by its place", () => {
    expect(wrong({ type: "scorecard", aggregate: "avg", measureColumn: "r" })).toBe(
      "r is a text column, and an average needs numbers. Pick the column to measure.",
    )
    const pivot = { type: "pivot" as const, pivotRows: [{ column: "r", title: "", grain: "" as const }], aggregate: "count" as const }
    expect(wrong({ ...pivot, aggregate2: "sum", measureColumn2: "a", aggregate3: "sum", measureColumn3: "day" })).toBe(
      "day is a date column, and a sum needs numbers. Pick the column for measure 3.",
    )
    expect(wrong({ type: "range", x: "day", aggregate: "min", measureColumn: "a", aggregate2: "avg", measureColumn2: "r" })).toBe(
      "r is a text column, and an average needs numbers. Pick the column for the high.",
    )
    expect(wrong({ type: "bullet", aggregate: "sum", measureColumn: "a", bulletTarget: "measure", aggregate2: "sum", measureColumn2: "ok" })).toBe(
      "ok is a yes-or-no column, and a sum needs numbers. Pick the column for the target.",
    )
  })

  // A lowest or a highest of anything but numbers is a value a table or a pivot table
  // shows as the column holds it; every other chart draws a number, so it would be blank.
  it("names a lowest or a highest of a column that holds no numbers, outside a table", () => {
    expect(wrong({ type: "bar", x: "r", aggregate: "max", measureColumn: "day" })).toBe(
      "day is a date column, and this chart draws the highest only of numbers: a table or a pivot table shows it as it is. Pick the column to measure.",
    )
    expect(wrong({ type: "range", x: "day", aggregate: "min", measureColumn: "r", aggregate2: "max", measureColumn2: "a" })).toBe(
      "r is a text column, and this chart draws the lowest only of numbers: a table or a pivot table shows it as it is. Pick the column for the low.",
    )
    expect(wrong({ type: "scorecard", aggregate: "max", measureColumn: "ok" })).toBe(
      "ok is a yes-or-no column, and this chart draws the highest only of numbers: a table or a pivot table shows it as it is. Pick the column to measure.",
    )
    expect(wrong({ type: "heatmap", x: "day", y: "r", aggregate: "min", measureColumn: "a" })).toBeNull()
    expect(wrong({ type: "table", columns: ["r"], aggregate: "min", measureColumn: "r" })).toBeNull()
    expect(wrong({ type: "pivot", pivotRows: [{ column: "r", title: "", grain: "" }], aggregate: "max", measureColumn: "day" })).toBeNull()
    const takes = (t: ChartType, a: ChartAggregate) => COLUMNS.filter(measureColumns(a, t)).map((c) => c.name)
    expect(takes("line", "max")).toEqual(takes("line", "sum"))
    expect(takes("table", "max")).toEqual(takes("table", "count"))
  })
})

describe("suggestedTypes", () => {
  const columns = [
    { name: "day", kind: "time", masked: false },
    { name: "region", kind: "text", masked: false },
    { name: "active", kind: "bool", masked: false },
    { name: "amount", kind: "number", masked: false },
  ]
  it("suggests a line for a time x and a bar for a text one, nothing otherwise", () => {
    expect(suggestedTypes(form({ x: "day" }), columns)).toEqual(["line"])
    expect(suggestedTypes(form({ x: "region" }), columns)).toEqual(["bar"])
    expect(suggestedTypes(form({ x: "active" }), columns)).toEqual(["bar"])
    expect(suggestedTypes(form({ x: "amount" }), columns)).toEqual([])
    expect(suggestedTypes(form({ x: "gone" }), columns)).toEqual([])
  })
})

describe("toNumber and labelOf", () => {
  it("reads a number or a database's numeric text, nothing else", () => {
    expect(toNumber(3)).toBe(3)
    expect(toNumber("12.50")).toBe(12.5)
    expect(toNumber("1e3")).toBe(1000)
    expect(toNumber("-0.5")).toBe(-0.5)
    expect(toNumber(".5")).toBe(0.5)
    expect(toNumber(" 7 ")).toBe(7)
    for (const v of ["abc", "", "0x10", "1,000", NaN, Infinity, null, undefined, true, {}]) expect(toNumber(v)).toBeNull()
  })

  it("names a midnight timestamp by its date, null as (empty), an object as its JSON", () => {
    expect(labelOf("2026-10-01T00:00:00Z")).toBe("2026-10-01")
    expect(labelOf("2026-10-01 00:00:00+00")).toBe("2026-10-01")
    expect(labelOf("2026-10-01T00:00:00.000Z")).toBe("2026-10-01")
    expect(labelOf("2026-10-01T13:00:00Z")).toBe("2026-10-01T13:00:00Z")
    expect(labelOf("2026-10-01T00:00:00+05:30")).toBe("2026-10-01T00:00:00+05:30")
    expect(labelOf(null)).toBe("(empty)")
    expect(labelOf(undefined)).toBe("(empty)")
    expect(labelOf({ a: 1 })).toBe('{"a":1}')
    expect(labelOf(5)).toBe("5")
    expect(labelOf(false)).toBe("false")
  })
})

describe("measureTitle and measureFormat", () => {
  const m = (measure: ChartField): ServerChartSettings => v2("bar", { x: [{ column: "d" }], measure: [measure] })
  it("names the measure by its title, else by what it counts or adds up", () => {
    expect(measureTitle(m({ aggregate: "count" }))).toBe("Count")
    expect(measureTitle(m({ aggregate: "count_distinct", column: "c" }))).toBe("Distinct c")
    expect(measureTitle(m({ aggregate: "sum", column: "c" }))).toBe("Sum of c")
    expect(measureTitle(m({ aggregate: "avg", column: "c" }))).toBe("Average c")
    expect(measureTitle(m({ aggregate: "min", column: "c" }))).toBe("Lowest c")
    expect(measureTitle(m({ aggregate: "max", column: "c" }))).toBe("Highest c")
    expect(measureTitle(m({ aggregate: "sum", column: "c", title: " Revenue " }))).toBe("Revenue")
    expect(measureTitle(v2("table", { columns: [{ column: "a" }] }))).toBe("")
  })

  it("writes a count in whole numbers and anything else with a decimal, unless a format is set", () => {
    expect(measureFormat(m({ aggregate: "count" }))).toBe("integer")
    expect(measureFormat(m({ aggregate: "count_distinct", column: "c" }))).toBe("integer")
    expect(measureFormat(m({ aggregate: "sum", column: "c" }))).toBe("decimal")
    expect(measureFormat(m({ aggregate: "avg", column: "c", format: "compact" }))).toBe("compact")
  })
})

describe("number formats", () => {
  // The gateway's formatChartNumber reads the same cases (workflow_chart_step_test.go), so a
  // workflow's message writes a number as the chart page does. A default rate of 0.081 was
  // drawn as 0.1 (no format said "this is a share"), and a decimal of 0.03 as 0
  // (KI-CHART-DECIMAL-AXIS-REPEATS-TICKS): a number under 1 keeps two significant digits.
  const golden = JSON.parse(readFileSync(resolve(__dirname, "../../../shared/chart_number_format_golden.json"), "utf8")) as {
    cases: { value: number; format: NumberFormat; text: string }[]
  }

  it("has cases of every format to check", () => {
    expect(new Set(golden.cases.map((c) => c.format))).toEqual(new Set(["integer", "decimal", "compact", "percent"]))
  })

  it.each(golden.cases.map((c) => [c.value, c.format, c.text, c] as const))("writes %s as %s %s, as the gateway does", (_v, _f, _t, c) => {
    expect(formatNumber(c.value, c.format)).toBe(c.text)
  })

  // A format the server accepts and the page cannot write breaks the chart; one the
  // builder does not offer can only be set by the assistant.
  it("writes, and offers in the builder, every format charts/settings.go accepts", () => {
    const go = readFileSync(`${SRC}/../../api-gateway/internal/charts/settings.go`, "utf8")
    const list = go.match(/NumberFormats\s*=\s*\[\]string\{([^}]*)\}/)
    expect(list, "charts.NumberFormats").not.toBeNull()
    const formats = [...list![1].matchAll(/"(\w+)"/g)].map((f) => f[1])
    expect(formats.length).toBeGreaterThan(0)
    const builder = readFileSync(`${SRC}/components/charts/ChartBuilder.tsx`, "utf8")
    for (const f of formats) {
      expect(formatNumber(1, f as NumberFormat), f).toMatch(/^1/)
      expect(builder, f).toContain(`<option value="${f}">`)
    }
  })
})

describe("assignSeriesColors", () => {
  const colors = (m: Map<string, string>) => Object.fromEntries(m)

  it("gives the rest the free tokens in name order", () => {
    expect(colors(assignSeriesColors(["US", "EU", "APAC"]))).toEqual({ APAC: "series-1", EU: "series-2", US: "series-3" })
  })

  it("keeps a pinned series' token, and a pin for a series not drawn still holds its token", () => {
    expect(colors(assignSeriesColors(["US", "EU", "APAC"], [{ value: "US", color: "series-1" }]))).toEqual({
      US: "series-1",
      APAC: "series-2",
      EU: "series-3",
    })
    expect(colors(assignSeriesColors(["EU", "US"], [{ value: "JP", color: "series-1" }]))).toEqual({
      EU: "series-2",
      US: "series-3",
    })
  })

  it("keeps the survivors' colours when a filter drops a pinned series", () => {
    const pins = [
      { value: "APAC", color: "series-1" },
      { value: "EU", color: "series-2" },
      { value: "US", color: "series-3" },
    ]
    expect(colors(assignSeriesColors(["US", "APAC"], pins))).toEqual({ APAC: "series-1", US: "series-3" })
  })

  it("gives a held token only when every other is taken, and ignores a pin that is not a series token", () => {
    const names = ["a", "b", "c", "d", "e", "f", "g"]
    const got = assignSeriesColors(names, [{ value: "zz", color: "series-1" }])
    expect(got.get("a")).toBe("series-2")
    expect(got.get("g")).toBe("series-1")
    expect(colors(assignSeriesColors(["EU"], [{ value: "EU", color: "#ff0000" }]))).toEqual({ EU: "series-1" })
    expect(colors(assignSeriesColors(["EU"], [{ value: "EU", color: "other" }]))).toEqual({ EU: "series-1" })
  })
})

const result = (columns: [alias: string, role: string][], rows: unknown[][], masked: string[] = []): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked,
  unfiltered: [],
  sql: "",
  read_at: "",
  execution_time_ms: 0,
})

const XY_COLUMNS: [string, string][] = [
  ["x", "the x"],
  ["s", "the series"],
  ["v", "the measure"],
]
/** A series chart's columns as the gateway reads them (chart_sql.go foldSeriesSQL). */
const FOLDED_COLUMNS: [string, string][] = [...XY_COLUMNS, ["rsync_other", ROLE_OTHER], ["rsync_series", ROLE_SERIES_COUNT]]

const DAY1 = "2026-10-01T00:00:00Z"
/**
 * Nine series on one day, a = 90 down to i = 10, as the gateway sends them: the largest
 * seven, then h and i folded into one Other row (its series NULL), every row saying 9.
 */
const nine = (other: number | null): unknown[][] => [
  ..."abcdefg".split("").map((name, i) => [DAY1, name, String(90 - i * 10), 0, 9]),
  [DAY1, null, other === null ? null : String(other), 1, 9],
]

describe("savedChartView: one series", () => {
  const bar = v2("bar", { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount", color: "series-3" }] })
  const rows = [
    ["2026-10-01T00:00:00Z", "12.5"],
    ["2026-10-02T00:00:00Z", null],
  ]

  it("draws the measure in its colour, by day, with what the chart is in words", () => {
    const view = savedChartView(bar, result([["x", "the x"], ["v", "the measure"]], rows))
    expect(view).toEqual({
      kind: "echart",
      settings: {
        type: "bar",
        stacked: false,
        description: "Sum of amount by day",
        x: { field: "x", label: "day", categories: ["2026-10-01", "2026-10-02"] },
        y: { field: "v", format: "decimal", title: "Sum of amount" },
        series: { field: undefined, values: [{ key: "", label: "Sum of amount", color: "series-3" }] },
      },
      rows: [
        { x: "2026-10-01", v: 12.5 },
        { x: "2026-10-02", v: null },
      ],
      series: [],
      notes: [],
    })
  })

  it("draws a line unstacked whatever the settings say", () => {
    const view = savedChartView({ ...bar, type: "line", stacked: true }, result([["x", "the x"], ["v", "the measure"]], rows))
    expect(view.kind === "echart" && view.settings).toMatchObject({ type: "line" })
    expect(view.kind === "echart" && "stacked" in view.settings).toBe(false)
  })

  it("says so when the measure is masked, or a column the chart needs is missing", () => {
    expect(savedChartView(bar, result([["x", "the x"], ["v", "the measure"]], rows, ["v"]))).toEqual({
      kind: "message",
      message: "This chart's measure is masked for you, so it is not drawn.",
    })
    expect(savedChartView(bar, result([["x", "the x"]], rows))).toEqual({
      kind: "message",
      message: "The result is missing a column the chart needs.",
    })
    const withSeries = { ...bar, roles: { ...bar.roles, series: [{ column: "region" }] } }
    expect(savedChartView(withSeries, result([["x", "the x"], ["v", "the measure"]], rows)).kind).toBe("message")
  })
})

describe("savedChartView: more than 7 series", () => {
  // The gateway keeps the largest 7 and folds the rest into Other in SQL (§ 14.3); the
  // page draws the rows it is sent and says how many more there were.
  type Aggregate = "sum" | "count" | "min" | "max" | "avg" | "count_distinct"
  const settings = (aggregate: Aggregate, colors?: ChartField["colors"]): ServerChartSettings =>
    v2("bar", {
      x: [{ column: "day" }],
      measure: [aggregate === "count" ? { aggregate } : { aggregate, column: "amount" }],
      series: [colors ? { column: "region", colors } : { column: "region" }],
    })
  const view = (aggregate: Aggregate, rows: unknown[][], colors?: ChartField["colors"]) => {
    const v = savedChartView(settings(aggregate, colors), result(FOLDED_COLUMNS, rows))
    if (v.kind !== "echart") throw new Error(`a ${v.kind}`)
    return v
  }
  const labels = (v: ReturnType<typeof view>) => ("series" in v.settings ? v.settings.series.values.map((s) => s.label) : [])
  const other = (v: ReturnType<typeof view>) => v.rows.find((r) => (r as { s: string }).s === "other") as { v: number } | undefined

  it("draws the 7 it is sent and their Other when the measure adds up", () => {
    const v = view("sum", nine(30))
    expect(labels(v)).toEqual(["a", "b", "c", "d", "e", "f", "g", "Other"])
    expect(other(v)?.v).toBe(30)
    expect(v.notes).toEqual(["2 more series are drawn as Other."])
    // Other is never pinned: only the drawn series are.
    expect(v.series.map((s) => s.value)).toEqual(["a", "b", "c", "d", "e", "f", "g"])
    expect(view("count", nine(30)).notes).toEqual(["2 more series are drawn as Other."])
    // Other comes last, after every kept series' rows.
    expect(v.rows.at(-1)).toEqual({ x: "2026-10-01", s: "other", v: 30 })
  })

  it("draws Other for a min or a max as the gateway worked it out", () => {
    expect(other(view("min", nine(10)))?.v).toBe(10)
    expect(other(view("max", nine(20)))?.v).toBe(20)
  })

  it("leaves the rest out, and says so, for an average or a distinct count", () => {
    for (const [aggregate, words] of [
      ["avg", "an average"],
      ["count_distinct", "a distinct count"],
    ] as const) {
      const v = view(aggregate, nine(null))
      expect(other(v)).toBeUndefined()
      expect(labels(v)).toHaveLength(7)
      expect(v.notes).toEqual([`2 more series are left out: ${words} cannot be added into Other.`])
    }
  })

  it("draws no Other on a day whose folded series had no value", () => {
    const v = view("sum", nine(null))
    expect(other(v)).toBeUndefined()
    expect(labels(v)).not.toContain("Other")
    expect(v.notes).toEqual(["2 more series are drawn as Other."])
  })

  it("gives a pin two series share to the larger one, and colours the rest by name", () => {
    // g comes first, and b is larger only by size (a sum of |value| across the days), so
    // neither row order nor a signed total gives b the pin.
    const kept = nine(30).slice(0, 7).map((r) => (r[1] === "b" ? [DAY1, "b", "-500", 0, 9] : r))
    const rows = [...kept.reverse(), nine(30)[7]]
    const pins = [
      { value: "g", color: "series-1" },
      { value: "b", color: "series-1" },
    ]
    const v = view("sum", rows, pins)
    const colors = Object.fromEntries(v.series.map((s) => [s.value, s.color]))
    expect(colors.b).toBe("series-1")
    expect(colors.g).not.toBe("series-1")
  })

  it("says one series past the cap in the singular", () => {
    const eight = (other: number | null) => nine(other).map((r) => [...r.slice(0, 4), 8])
    expect(view("sum", eight(30)).notes).toEqual(["1 more series is drawn as Other."])
    expect(view("avg", eight(null)).notes).toEqual(["1 more series is left out: an average cannot be added into Other."])
    expect(view("count_distinct", eight(null)).notes).toEqual(["1 more series is left out: a distinct count cannot be added into Other."])
  })

  it("says nothing about Other with 7 series or fewer, and draws a gateway's rows without the flag as they come", () => {
    const seven = nine(30).slice(0, 7).map((r) => [...r.slice(0, 4), 7])
    expect(view("sum", seven).notes).toEqual([])
    // A gateway from before § 14.3 sends no flag: every row is a series of its own.
    const old = "abcdefghi".split("").map((name, i) => [DAY1, name, String(90 - i * 10)])
    const v = savedChartView(settings("sum"), result(XY_COLUMNS, old))
    expect(v.kind === "echart" && v.series.map((s) => s.value)).toEqual("abcdefghi".split(""))
    expect(v.kind === "echart" && v.notes).toEqual([])
  })
})

describe("grainLabel", () => {
  it("names a bucket by its grain, and the start date where the year does not start in January", () => {
    expect(grainLabel("2026-10-01T00:00:00Z", "month")).toBe("2026-10")
    expect(grainLabel("2026-10-01", "quarter")).toBe("2026 Q4")
    expect(grainLabel("2026-01-01 00:00:00+00", "quarter")).toBe("2026 Q1")
    expect(grainLabel("2026-01-01", "year")).toBe("2026")
    expect(grainLabel("2026-09-28T00:00:00Z", "week")).toBe("2026-09-28")
    expect(grainLabel("2026-10-07T00:00:00Z", "day")).toBe("2026-10-07")
    // A fiscal year starting in February: its quarters and years start off the calendar's.
    expect(grainLabel("2026-02-01", "quarter")).toBe("2026-02-01")
    expect(grainLabel("2026-04-15", "quarter")).toBe("2026-04-15")
    expect(grainLabel("2026-02-01", "year")).toBe("2026-02-01")
  })

  it("names anything else as labelOf does", () => {
    expect(grainLabel("2026-10-01T00:00:00Z", undefined)).toBe("2026-10-01")
    expect(grainLabel(null, "month")).toBe("(empty)")
    expect(grainLabel("EU", "month")).toBe("EU")
    expect(grainLabel(2026, "year")).toBe("2026")
  })

  it("draws a grained x by its bucket's name", () => {
    const line = v2("line", { x: [{ column: "day", grain: "quarter" }], measure: [{ aggregate: "count" }] })
    const v = savedChartView(line, result([["x", "the x"], ["v", "the measure"]], [["2026-07-01T00:00:00Z", 4], ["2026-10-01T00:00:00Z", 6]]))
    expect(v.kind === "echart" && "x" in v.settings && v.settings.x.categories).toEqual(["2026 Q3", "2026 Q4"])
  })
})

describe("savedChartView: a line's or an area's missing periods", () => {
  const XV: [string, string][] = [
    ["x", "the x"],
    ["v", "the measure"],
  ]
  const drawn = (s: ServerChartSettings, r: ChartResult) => {
    const v = savedChartView(s, r)
    if (v.kind !== "echart" || !("x" in v.settings)) throw new Error(`not an x chart: ${v.kind}`)
    return { categories: v.settings.x.categories, rows: v.rows }
  }
  const line = (grain?: ChartGrain, rest: Partial<ServerChartSettings> = {}) =>
    v2("line", { x: [{ column: "day", ...(grain ? { grain } : {}) }], measure: [{ aggregate: "count" }] }, rest)

  it("puts back each period with no rows between the first and the last, with no value, so it draws as a gap", () => {
    const day = drawn(line("day"), result(XV, [["2026-10-07T00:00:00Z", 3], ["2026-10-08T00:00:00Z", 4], ["2026-10-10T00:00:00Z", 5]]))
    expect(day.categories).toEqual(["2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10"])
    expect(day.rows).toEqual([
      { x: "2026-10-07", v: 3 },
      { x: "2026-10-08", v: 4 },
      { x: "2026-10-09", v: null },
      { x: "2026-10-10", v: 5 },
    ])
    const periods = (grain: ChartGrain, days: string[]) => drawn(line(grain), result(XV, days.map((d) => [d, 1]))).categories
    expect(periods("week", ["2026-09-28", "2026-10-12"])).toEqual(["2026-09-28", "2026-10-05", "2026-10-12"])
    expect(periods("month", ["2026-01-01", "2026-04-01"])).toEqual(["2026-01", "2026-02", "2026-03", "2026-04"])
    expect(periods("quarter", ["2026-01-01", "2026-07-01"])).toEqual(["2026 Q1", "2026 Q2", "2026 Q3"])
    expect(periods("year", ["2024-01-01", "2026-01-01"])).toEqual(["2024", "2025", "2026"])
    // A fiscal quarter starts off the calendar's, and its periods step from where it starts.
    expect(periods("quarter", ["2026-02-01", "2026-08-01"])).toEqual(["2026-02-01", "2026-05-01", "2026-08-01"])
  })

  it("puts the periods back newest first on a chart sorted so, and gives each series and each measure no value", () => {
    expect(drawn(line("day", { sort: "x_desc" }), result(XV, [["2026-10-10", 1], ["2026-10-08", 2]])).categories).toEqual([
      "2026-10-10",
      "2026-10-09",
      "2026-10-08",
    ])
    const area = v2("area", { x: [{ column: "day", grain: "day" }], measure: [{ aggregate: "count" }], series: [{ column: "region" }] })
    const split = drawn(area, result(XY_COLUMNS, [["2026-10-07", "EU", 1], ["2026-10-07", "US", 2], ["2026-10-09", "EU", 3]]))
    expect(split.categories).toEqual(["2026-10-07", "2026-10-08", "2026-10-09"])
    expect(split.rows.filter((r) => (r as { x: string }).x === "2026-10-08")).toEqual([
      { x: "2026-10-08", s: "s0", v: null },
      { x: "2026-10-08", s: "s1", v: null },
    ])
    const combo = v2("line", {
      x: [{ column: "day", grain: "month" }],
      measure: [{ aggregate: "count" }, { aggregate: "sum", column: "amount", mark: "bar" }],
    })
    const both = drawn(combo, result([...XV, ["amount", ROLE_SECOND_MEASURE]], [["2026-01-01", 1, 10], ["2026-03-01", 2, 20]]))
    expect(both.rows).toEqual([
      { x: "2026-01", v: 1, v2: 10 },
      { x: "2026-02", v: null, v2: null },
      { x: "2026-03", v: 2, v2: 20 },
    ])
  })

  it("puts back a missing day on a daily date x with no grain, the builder's default", () => {
    // Prod sends a DATE as "2026-10-08"; a midnight time reads as its day.
    const daily = [["2026-10-07", 3], ["2026-10-08", 4], ["2026-10-10", 5]]
    const days = ["2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10"]
    expect(drawn(line(), result(XV, daily)).categories).toEqual(days)
    expect(drawn(line(), result(XV, daily)).rows).toContainEqual({ x: "2026-10-09", v: null })
    const midnight = daily.map(([d, v]) => [`${d}T00:00:00Z`, v])
    expect(drawn(line(), result(XV, midnight)).categories).toEqual(days)
    expect(drawn(line(undefined, { sort: "x_desc" }), result(XV, [...daily].reverse())).categories).toEqual([...days].reverse())
  })

  it("puts nothing back on a cut result, a bar, a value sort, past the row cap, or a date x with no grain that is not daily", () => {
    const gappy = [["2026-10-07", 3], ["2026-10-09", 5]]
    const two = ["2026-10-07", "2026-10-09"]
    expect(drawn(line("day"), { ...result(XV, gappy), truncated: true }).categories).toEqual(two)
    // With no grain, no two days a day apart: weekly or monthly dates are joined as they are.
    expect(drawn(line(), result(XV, gappy)).categories).toEqual(two)
    const weekly = ["2026-10-05", "2026-10-12", "2026-10-26"]
    expect(drawn(line(), result(XV, weekly.map((d) => [d, 1]))).categories).toEqual(weekly)
    // Scattered dates, one pair a day apart, are not daily.
    const scattered = ["2026-09-01", "2026-09-02", "2026-09-20", "2026-10-11"]
    expect(drawn(line(), result(XV, scattered.map((d) => [d, 1]))).categories).toEqual(scattered)
    // A time of day is no day: each value is its own point.
    const times = ["2026-10-07T09:00:00Z", "2026-10-08T09:00:00Z", "2026-10-10T09:00:00Z"]
    expect(drawn(line(), result(XV, times.map((d) => [d, 1]))).categories).toHaveLength(3)
    expect(drawn({ ...line(), type: "bar" }, result(XV, [["2026-10-07", 3], ["2026-10-08", 4], ["2026-10-10", 5]])).categories).toHaveLength(3)
    expect(drawn({ ...line("day"), type: "bar" }, result(XV, gappy)).categories).toEqual(two)
    expect(drawn(line("day", { sort: "value_asc" }), result(XV, gappy)).categories).toEqual(two)
    // Over 36 years of days: more periods than a line draws.
    expect(drawn(line("day"), result(XV, [["1990-01-01", 1], ["2026-10-09", 2]])).categories).toEqual(["1990-01-01", "2026-10-09"])
  })

  // A line with a grain and no rows threw (the first of no days was read), and the throw
  // took down the whole dashboard page, not its tile: with no dates, nothing is put back.
  it("draws a line or an area with a grain and no dated rows as it comes", () => {
    expect(drawn(line("day"), result(XV, [])).categories).toEqual([])
    expect(drawn({ ...line("month"), type: "area" }, result(XV, [])).categories).toEqual([])
    // Only a NULL date: no dated stretch to lay out.
    expect(drawn(line("day"), result(XV, [[null, 1]])).categories).toEqual(["(empty)"])
  })
})

describe("savedChartView: scorecard, table and heatmap", () => {
  it("shows a scorecard's number and the earlier period's", () => {
    const card = v2("scorecard", { measure: [{ aggregate: "count" }], date: [{ column: "day" }] }, { compare: { days: 7 } })
    expect(savedChartView(card, result([["count", "the measure"], ["previous", "the earlier period"]], [[120, "100"]]))).toEqual({
      kind: "scorecard",
      label: "Count",
      value: 120,
      format: "integer",
      previous: 100,
      days: 7,
      notes: [],
    })
    expect(savedChartView(v2("scorecard", { measure: [{ aggregate: "count" }] }), result([["count", "the measure"]], [[3]]))).toEqual({
      kind: "scorecard",
      label: "Count",
      value: 3,
      format: "integer",
      notes: [],
    })
    expect(savedChartView(card, result([["count", "the measure"]], [["***"]], ["count"])).kind).toBe("message")
  })

  it("writes a table's measure in its format and leaves a masked one as the server sent it", () => {
    const table = v2(
      "table",
      { columns: [{ column: "region" }, { column: "day" }], measure: [{ aggregate: "sum", column: "amount", format: "compact" }] },
      { sort: "value_desc" },
    )
    const columns: [string, string][] = [
      ["region", "table column region"],
      ["day", "table column day"],
      ["amount", "the measure"],
    ]
    const rows = [
      ["EU", "2026-10-01T00:00:00Z", "12500"],
      [null, "2026-10-02", null],
    ]
    expect(savedChartView(table, result(columns, rows))).toEqual({
      kind: "table",
      head: ["region", "day", "Sum of amount"],
      body: [
        ["EU", "2026-10-01", "12.5K"],
        ["", "2026-10-02", ""],
      ],
      numeric: [false, false, true],
      notes: [],
    })
    const masked = savedChartView(table, result(columns, [["EU", "2026-10-01", "****"]], ["amount"]))
    expect(masked.kind === "table" && masked.body[0][2]).toBe("****")
  })

  // Re-verify of #1540: a table's or a pivot's lowest or highest of dates or text was a
  // blank cell. It reads as the column holds it, and a download defuses it like any text.
  it("shows a table's and a pivot's lowest or highest of dates or text as the column holds it", () => {
    const table = v2("table", { columns: [{ column: "region" }], measure: [{ aggregate: "max", column: "status" }] })
    const columns: [string, string][] = [
      ["region", "table column region"],
      ["status", "the measure"],
    ]
    const rows = [
      ["EU", "2026-10-07T14:03:00Z"],
      ["US", "=HYPERLINK(1)"],
      ["APAC", null],
    ]
    const view = savedChartView(table, result(columns, rows))
    expect(view.kind === "table" && view.body).toEqual([
      ["EU", "2026-10-07 14:03 UTC"],
      ["US", "=HYPERLINK(1)"],
      ["APAC", ""],
    ])
    expect(view.kind === "table" && view.numeric).toEqual([false, false])
    const exported = tableExport(table, result(columns, rows))
    expect(exported.numeric).toEqual([false, false])
    expect(toCsv(exported.head, exported.body, exported.numeric)).toContain("US,'=HYPERLINK(1)")
    // A measure of numbers still lines up, and downloads, as one.
    expect(tableExport(table, result(columns, [["EU", "12"], ["US", null]])).numeric).toEqual([false, true])

    const pivot = v2("pivot", {
      rows: [{ column: "region" }],
      measure: [{ aggregate: "min", column: "status" }, { aggregate: "count" }],
    })
    const pivotColumns: [string, string][] = [
      ["region", pivotRole("row", 1)],
      ["region_total", pivotTotalRole("row", 1)],
      ["status", "the measure"],
      ["n", ROLE_SECOND_MEASURE],
    ]
    const pivotRows = [
      ["EU", 0, "closed", 3],
      [null, 1, "closed", 3],
    ]
    const drawn = savedChartView(pivot, result(pivotColumns, pivotRows))
    expect(drawn.kind === "table" && drawn.body).toEqual([
      ["EU", "closed", "3"],
      ["Total", "closed", "3"],
    ])
    expect(tableExport(pivot, result(pivotColumns, pivotRows)).numeric).toEqual([false, false, true])
  })

  // Review: a table whose order the engine refused, or took too long over, is drawn from
  // its statement without that order; it says its rows may come in another order.
  it("says a table drawn without the order of each column may come in another order", () => {
    const table = v2("table", { columns: [{ column: "region" }, { column: "tags" }] })
    const columns: [string, string][] = [
      ["region", "table column region"],
      ["tags", "table column tags"],
    ]
    const drawn = savedChartView(table, { ...result(columns, [["EU", "[1,2]"]]), unordered: true })
    expect(drawn.kind === "table" && drawn.notes).toEqual([
      "Rows may come in a different order each time: the table could not be sorted by all its columns.",
    ])
    const sorted = savedChartView(table, result(columns, [["EU", "[1,2]"]]))
    expect(sorted.kind === "table" && sorted.notes).toEqual([])
  })

  // Launch review bug 16: a table tile showed "2026-10-07T14:03:00.123456Z" as the
  // database sent it, and its number columns sat on the left under a right-aligned measure.
  it("reads a table's timestamps as day and time, and lines up a column of numbers on the right, on screen only", () => {
    const table = v2("table", { columns: [{ column: "region" }, { column: "seen_at" }, { column: "orders" }] })
    const columns: [string, string][] = [
      ["region", "table column region"],
      ["seen_at", "table column seen_at"],
      ["orders", "table column orders"],
    ]
    const rows = [
      ["EU", "2026-10-07T14:03:00.123456Z", 12],
      ["US", "2026-10-07 09:15:42+00:00", "7.5"],
      ["APAC", "2026-10-07T23:59:00+05:30", null],
      ["LATAM", "2026-10-07T00:00:00Z", 3],
      ["MEA", "2026-10-07T08:00:00", 1],
    ]
    const view = savedChartView(table, result(columns, rows))
    expect(view.kind === "table" && view.body.map((r) => r[1])).toEqual([
      "2026-10-07 14:03 UTC",
      "2026-10-07 09:15:42 UTC",
      "2026-10-07 23:59 +05:30",
      "2026-10-07",
      "2026-10-07 08:00",
    ])
    // A column of numbers lines up like the measure, written as the source sent it.
    expect(view.kind === "table" && view.numeric).toEqual([false, false, true])
    expect(view.kind === "table" && view.body.map((r) => r[2])).toEqual(["12", "7.5", "", "3", "1"])
    // Text is never a number column, nor is a column with no values at all.
    const empty = savedChartView(table, result(columns, [["EU", null, null]]))
    expect(empty.kind === "table" && empty.numeric).toEqual([false, false, false])

    // A download keeps each value as the source sent it, and flags only a measure as a
    // number: csv.ts defuses every cell not flagged, so a "=1+1" in a column of text stays inert.
    expect(tableExport(table, result(columns, rows))).toEqual({
      head: ["region", "seen_at", "orders"],
      body: [
        ["EU", "2026-10-07T14:03:00.123456Z", "12"],
        ["US", "2026-10-07 09:15:42+00:00", "7.5"],
        ["APAC", "2026-10-07T23:59:00+05:30", ""],
        ["LATAM", "2026-10-07", "3"],
        ["MEA", "2026-10-07T08:00:00", "1"],
      ],
      numeric: [false, false, false],
    })
  })

  it("lays a heatmap's y categories out by name, in its colour", () => {
    const heat = v2("heatmap", {
      x: [{ column: "day" }],
      y: [{ column: "region", title: "Region" }],
      measure: [{ aggregate: "count", color: "series-4" }],
    })
    const rows = [
      ["d1", "US", 1],
      ["d1", "EU", 2],
      ["d2", "APAC", 3],
    ]
    const view = savedChartView(heat, result([["x", "the x"], ["y", "the y"], ["count", "the measure"]], rows))
    expect(view.kind === "echart" && view.settings).toEqual({
      type: "heatmap",
      description: "Count by day and Region",
      x: { field: "x", label: "day", categories: ["d1", "d2"] },
      y: { field: "y", label: "Region", categories: ["APAC", "EU", "US"] },
      value: { field: "v", label: "Count", format: "integer" },
      color: "series-4",
    })
    expect(view.kind === "echart" && view.rows[0]).toEqual({ x: "d1", y: "US", v: 1 })
    expect(view.kind === "echart" && view.notes).toEqual([])
  })

  // The rows come by x, then y: the cap cuts inside the last x drawn, so a blank cell
  // there may be one past the cap rather than one with no rows.
  it("says the last x column of a cut heatmap may have values its blank cells do not show", () => {
    const heat = v2("heatmap", { x: [{ column: "day" }], y: [{ column: "region" }], measure: [{ aggregate: "count" }] })
    const cols: [string, string][] = [["x", "the x"], ["y", "the y"], ["count", "the measure"]]
    const rows = [
      ["d1", "EU", 2],
      ["d1", "US", 1],
      ["d2", "APAC", 3],
    ]
    const cut = savedChartView(heat, { ...result(cols, rows), truncated: true })
    expect(cut.kind === "echart" && cut.notes).toEqual([
      "The cap cut the rows inside d2: its blank cells may be past the cap, not empty.",
    ])
  })
})

describe("changeText", () => {
  it("says the change on the earlier period in words", () => {
    expect(changeText(110, 100, 7)).toBe("+10% on the 7 days before")
    expect(changeText(112.4, 100, 7)).toBe("+12% on the 7 days before")
    expect(changeText(95, 100, 7)).toBe("−5.0% on the 7 days before")
    expect(changeText(100, 100, 1)).toBe("No change on the day before")
    // Less negative is up.
    expect(changeText(-50, -100, 30)).toBe("+50% on the 30 days before")
  })

  it("says what it can when there is no earlier number or it was 0", () => {
    expect(changeText(5, 0, 30)).toBe("Up from 0 on the 30 days before")
    expect(changeText(0, 0, 7)).toBe("No change on the 7 days before")
    expect(changeText(null, 100, 7)).toBe("No number for the 7 days before")
    expect(changeText(5, undefined, 90)).toBe("No number for the 90 days before")
    expect(changeText(5, null, 365)).toBe("No number for the 365 days before")
    expect(changeText(5, null, 1)).toBe("No number for the day before")
  })
})

describe("changeShort", () => {
  // A tile has no room for the sentence: the change goes beside the number, the sentence on hover.
  it("says the change in a few characters, the same numbers as the sentence", () => {
    expect(changeShort(110, 100)).toBe("+10%")
    expect(changeShort(112.4, 100)).toBe("+12%")
    expect(changeShort(95, 100)).toBe("−5.0%")
    expect(changeShort(0, 5)).toBe("−100%")
    expect(changeShort(100, 100)).toBe("No change")
    expect(changeShort(-50, -100)).toBe("+50%")
    expect(changeShort(5, 0)).toBe("Up from 0")
    expect(changeShort(0, 0)).toBe("No change")
    // A change of thousands of percent is grouped as every other number on the chart is.
    expect(changeShort(12345, 1)).toBe("+1,234,400%")
    expect(changeShort(-12345, 1)).toBe("−1,234,600%")
  })

  it("has nothing to say without both numbers", () => {
    expect(changeShort(null, 100)).toBeNull()
    expect(changeShort(5, undefined)).toBeNull()
    expect(changeShort(5, null)).toBeNull()
  })
})

describe("the notes on a chart cut at its type's caps", () => {
  // The gateway's chartTruncatedNote and chartOtherNote read the same cases
  // (workflow_chart_step_test.go), so a workflow's message says what was left out as the
  // chart page does.
  const golden = JSON.parse(readFileSync(resolve(__dirname, "../../../shared/chart_notes_golden.json"), "utf8")) as {
    truncated: { type: string; note: string }[]
    other: { type: string; aggregate: ChartAggregate; rest: number; note: string }[]
  }

  it("has cases to check", () => {
    expect(golden.truncated.length).toBeGreaterThan(0)
    expect(golden.other.length).toBeGreaterThan(0)
  })

  it.each(golden.truncated.map((c) => [c.type || "no type", c] as const))("says a %s's rows were cut as the gateway does", (_, c) => {
    expect(truncatedNote(c.type || undefined)).toBe(c.note)
  })

  it.each(golden.other.map((c) => [c.note || "nothing", c] as const))("says %s as the gateway does", (_, c) => {
    expect(otherNote(c.type, c.aggregate, c.rest)).toBe(c.note || null)
  })
})

describe("binLabel", () => {
  // The gateway's chartBinLabel reads the same cases (workflow_chart_step_test.go), so a
  // workflow's message names a bin as the chart page does.
  const golden = JSON.parse(readFileSync(resolve(__dirname, "../../../shared/chart_bin_labels_golden.json"), "utf8")) as {
    cases: { lo: number; width: number; label: string }[]
  }

  it("has cases to check", () => expect(golden.cases.length).toBeGreaterThan(0))

  it.each(golden.cases.map((c) => [c.label, c] as const))("writes %s as the gateway does", (_, c) => {
    expect(binLabel(c.lo, c.width)).toBe(c.label)
  })

  // Automatic bins are (max − min) / 20 wide, so their edges are unround: prod labelled one
  // 2.4–8074.6165 (KI-CHART-HISTOGRAM-BIN-LABELS-UNROUNDED). An edge keeps only the width's
  // decimals, to the width's third significant digit, and neighbouring bins still differ.
  it.each([8072.2165, 403.611, 1.23456, 0.0123456, 0.09996, 1.25, 0.125, 7, 1e-7, 2.4999999999])(
    "labels 20 bins %s wide apart, with no more decimals than the width's third significant digit",
    (width) => {
      const most = Math.max(0, 2 - Math.floor(Math.log10(width)))
      for (const first of [2.4, -3.3, 0]) {
        const labels = Array.from({ length: 20 }, (_, k) => binLabel(first + k * width, width))
        expect(new Set(labels).size, labels.join(" ")).toBe(20)
        for (const edge of labels.flatMap((l) => l.split("–"))) {
          expect(edge).not.toBe("-0")
          expect(edge.split(".")[1]?.length ?? 0, `${edge} of ${labels.join(" ")}`).toBeLessThanOrEqual(most)
        }
      }
    },
  )
})

describe("savedChartView: the D4b types", () => {
  const HISTOGRAM_COLUMNS: [string, string][] = [
    ["lo", "the x"],
    ["width", "the bin width"],
    ["n", "the measure"],
  ]
  const histogram = v2("histogram", { x: [{ column: "amount", bins: 4 }] })
  const echart = (v: ReturnType<typeof savedChartView>) => {
    if (v.kind !== "echart") throw new Error(`a ${v.kind}`)
    return v
  }

  it("draws a histogram's empty bins between the ones a row fell in, least to greatest", () => {
    const v = echart(savedChartView(histogram, result(HISTOGRAM_COLUMNS, [["0", "10", 3], ["30", "10", "1"]])))
    expect(v.settings).toMatchObject({
      type: "histogram",
      description: "Count by amount",
      x: { categories: ["0–10", "10–20", "20–30", "30–40"] },
      y: { format: "integer", title: "Count" },
      // Each bin's least edge, for adding neighbours up where the chart is too narrow.
      bins: { lo: [0, 10, 20, 30], width: 10 },
    })
    expect(v.rows).toEqual([
      { x: "0–10", v: 3 },
      { x: "10–20", v: 0 },
      { x: "20–30", v: 0 },
      { x: "30–40", v: 1 },
    ])
    expect(v.notes).toEqual([])
  })

  it("draws one bar for a column that holds one number, and leaves empty bins out past the rows a chart draws", () => {
    const one = echart(savedChartView(histogram, result(HISTOGRAM_COLUMNS, [["5", "0", 4]])))
    expect(one.rows).toEqual([{ x: "5", v: 4 }])
    expect(one.settings).not.toHaveProperty("bins")
    const wide = echart(savedChartView(histogram, result(HISTOGRAM_COLUMNS, [["0", "1", 2], ["1000000", "1", 1]])))
    expect(wide.rows).toEqual([
      { x: "0–1", v: 2 },
      { x: "1,000,000–1,000,001", v: 1 },
    ])
    expect(wide.notes).toEqual(["Bins no row fell in are left out: there are too many to draw."])
    expect(wide.settings).toMatchObject({ bins: { lo: [0, 1_000_000], width: 1 } })
  })

  it("draws no histogram when its column is masked", () => {
    expect(savedChartView(histogram, result(HISTOGRAM_COLUMNS, [["***", "10", 3]], ["lo"]))).toEqual({
      kind: "message",
      message: "This chart's column is masked for you, so it is not drawn.",
    })
  })

  const PIE_COLUMNS: [string, string][] = [
    ["s", "the series"],
    ["v", "the measure"],
    ["rsync_other", ROLE_OTHER],
    ["rsync_series", ROLE_SERIES_COUNT],
  ]
  const pie = v2("pie", { series: [{ column: "plan" }], measure: [{ aggregate: "sum", column: "amount" }] })

  it("draws a pie's slices in the server's order, and leaves out one of zero or below", () => {
    const v = echart(
      savedChartView(
        pie,
        result(PIE_COLUMNS, [
          ["Pro", "50", 0, 4],
          ["Free", "30", 0, 4],
          ["Team", "0", 0, 4],
          ["Trial", "-5", 0, 4],
        ]),
      ),
    )
    expect(v.settings).toMatchObject({
      type: "pie",
      description: "Sum of amount by plan",
      slices: { label: "plan", values: [{ label: "Pro" }, { label: "Free" }] },
      value: { label: "Sum of amount", format: "decimal" },
    })
    expect(v.rows).toEqual([
      { s: "s0", v: 50 },
      { s: "s1", v: 30 },
    ])
    expect(v.notes).toEqual(["2 slices are zero or below, so they are not drawn."])
    expect(v.series.map((s) => s.value)).toEqual(["Pro", "Free"])
  })

  it("draws Other last, never pinned, and says how many slices it holds", () => {
    const v = echart(savedChartView({ ...pie, type: "donut" }, result(PIE_COLUMNS, nine(30).map(([, ...cells]) => cells))))
    expect(v.settings.type).toBe("donut")
    expect("slices" in v.settings && v.settings.slices.values.map((s) => s.label)).toEqual(["a", "b", "c", "d", "e", "f", "g", "Other"])
    expect(v.rows.at(-1)).toEqual({ s: "other", v: 30 })
    expect(v.series.map((s) => s.value)).toEqual(["a", "b", "c", "d", "e", "f", "g"])
    // The builder calls a pie's series its slices, so the note does too.
    expect(v.notes).toEqual(["2 more slices are drawn as Other."])
  })

  it("names what is past a pie's cap slices, one of them in the singular, and an average's left out", () => {
    const eight = (other: number | null) => nine(other).map(([, ...cells]) => cells.map((c, i) => (i === 3 ? 8 : c)))
    expect(echart(savedChartView(pie, result(PIE_COLUMNS, eight(30)))).notes).toEqual(["1 more slice is drawn as Other."])
    const avg = v2("pie", { series: [{ column: "plan" }], measure: [{ aggregate: "avg", column: "amount" }] })
    expect(echart(savedChartView(avg, result(PIE_COLUMNS, nine(null).map(([, ...cells]) => cells)))).notes).toEqual([
      "2 more slices are left out: an average cannot be added into Other.",
    ])
    expect(echart(savedChartView(avg, result(PIE_COLUMNS, eight(null)))).notes).toEqual([
      "1 more slice is left out: an average cannot be added into Other.",
    ])
  })

  const COMBO_COLUMNS: [string, string][] = [
    ["x", "the x"],
    ["v", "the measure"],
    ["v2", "the second measure"],
  ]
  const combo = (second: ChartField) =>
    v2("bar", { x: [{ column: "day" }], measure: [{ aggregate: "count", color: "series-2" }, second] })

  it("draws a combo's two measures in two colours, each with its mark, its axis and its format", () => {
    const v = echart(
      savedChartView(
        combo({ aggregate: "sum", column: "amount", mark: "line", axis: "right", color: "series-2" }),
        result(COMBO_COLUMNS, [[DAY1, 3, "12.5"]]),
      ),
    )
    expect(v.settings).toMatchObject({
      type: "bar",
      description: "Count and Sum of amount by day",
      y: { title: "Count" },
      y2: { format: "decimal", title: "Sum of amount" },
    })
    // The second asked for the first's colour: it takes the next free one instead.
    expect("series" in v.settings && v.settings.series.values).toEqual([
      { key: "", label: "Count", color: "series-2", value: "v", format: "integer" },
      { key: "2", label: "Sum of amount", color: "series-1", value: "v2", mark: "line", axis: "right", format: "decimal" },
    ])
    expect(v.rows).toEqual([{ x: "2026-10-01", v: 3, v2: 12.5 }])
  })

  it("titles the left axis with both measures when neither is on the right, and draws no combo with one masked", () => {
    const left = echart(savedChartView(combo({ aggregate: "max", column: "amount" }), result(COMBO_COLUMNS, [[DAY1, 3, 9]])))
    expect(left.settings).toMatchObject({ y: { title: "Count · Highest amount" } })
    expect("y2" in left.settings).toBe(false)
    expect(savedChartView(combo({ aggregate: "max", column: "amount" }), result(COMBO_COLUMNS, [[DAY1, 3, "***"]], ["v2"])).kind).toBe(
      "message",
    )
  })

  it("draws 100% only on a stacked chart", () => {
    const shares = v2("area", { x: [{ column: "day" }], measure: [{ aggregate: "count" }], series: [{ column: "region" }] }, { percent: true })
    const rows = [[DAY1, "EU", 3]]
    expect(echart(savedChartView({ ...shares, stacked: true }, result(XY_COLUMNS, rows))).settings).toMatchObject({ stacked: true, percent: true })
    expect("percent" in echart(savedChartView(shares, result(XY_COLUMNS, rows))).settings).toBe(false)
  })

  it("draws a waterfall's Total only from every row", () => {
    const waterfall = v2("waterfall", { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "net" }] })
    const columns: [string, string][] = [["x", "the x"], ["v", "the measure"]]
    const whole = echart(savedChartView(waterfall, result(columns, [[DAY1, "4"]])))
    expect(whole.settings).toMatchObject({ type: "waterfall", total: true })
    expect(whole.notes).toEqual([])
    const cut = echart(savedChartView(waterfall, { ...result(columns, [[DAY1, "4"]]), truncated: true }))
    expect(cut.settings).toMatchObject({ type: "waterfall", total: false })
    expect(cut.notes).toEqual(["No Total bar: the source has more rows than were drawn."])
  })

  // Spaced a point apart, a day with no rows drew the days either side of it a day apart
  // (the prod review of #1540): each period is a point, one with no value a break.
  it("shows a scorecard's sparkline when it asks for one, a point per period, one with no value a break", () => {
    const card = v2("scorecard", { measure: [{ aggregate: "count" }], date: [{ column: "day" }] }, { compare: { days: 7 }, sparkline: true })
    const columns: [string, string][] = [["count", "the measure"], ["previous", "the earlier period"]]
    const spark = [
      ["2026-10-01T00:00:00Z", "5"],
      ["2026-10-02T00:00:00Z", null],
      ["2026-10-03T00:00:00Z", 7],
      ["2026-10-06T00:00:00Z", 4],
    ]
    expect(savedChartView(card, { ...result(columns, [[12, 10]]), spark })).toMatchObject({
      kind: "scorecard",
      spark: [
        { date: "2026-10-01", value: 5 },
        { date: "2026-10-02", value: null },
        { date: "2026-10-03", value: 7 },
        { date: "2026-10-04", value: null },
        { date: "2026-10-05", value: null },
        { date: "2026-10-06", value: 4 },
      ],
    })
    // Past 30 days a point is a week, past 90 a month (sparkGrain, chart_sql.go).
    const weeks = savedChartView({ ...card, compare: { days: 60 } }, { ...result(columns, [[12, 10]]), spark: [["2026-08-03", 1], ["2026-08-17", 2]] })
    expect(weeks).toMatchObject({ spark: [{ date: "2026-08-03", value: 1 }, { date: "2026-08-10", value: null }, { date: "2026-08-17", value: 2 }] })
    const months = savedChartView({ ...card, compare: { days: 365 } }, { ...result(columns, [[12, 10]]), spark: [["2026-01-01", 1], ["2026-03-01", 2]] })
    expect(months).toMatchObject({ spark: [{ date: "2026-01-01", value: 1 }, { date: "2026-02-01", value: null }, { date: "2026-03-01", value: 2 }] })
    // Dates that are not a grain's periods (an engine's own week start) are drawn as they come.
    const odd = savedChartView({ ...card, compare: { days: 60 } }, { ...result(columns, [[12, 10]]), spark: [["2026-08-03", 1], ["2026-08-12", 2]] })
    expect(odd).toMatchObject({ spark: [{ date: "2026-08-03", value: 1 }, { date: "2026-08-12", value: 2 }] })
    // No rows in the window: no points, where the first of no days was read and threw.
    expect(savedChartView(card, { ...result(columns, [[0, 0]]), spark: [] })).toMatchObject({ kind: "scorecard", spark: [] })
    // Without the setting, or when the server sent none (a masked column), there is none.
    expect("spark" in savedChartView({ ...card, sparkline: undefined }, { ...result(columns, [[12, 10]]), spark })).toBe(false)
    expect("spark" in savedChartView(card, result(columns, [[12, 10]]))).toBe(false)
  })
})

describe("the D4c types' fields", () => {
  it("sends a scatter its x, y, size and colour, and reads them back", () => {
    const f = form({ type: "bubble", x: "amount", xTitle: "Amount", y: "qty", size: "discount", sizeTitle: "Discount", series: "region" })
    const s = toSettings(f)
    expect(s.roles).toEqual({
      x: [{ column: "amount", title: "Amount" }],
      y: [{ column: "qty" }],
      size: [{ column: "discount", title: "Discount" }],
      series: [{ column: "region" }],
    })
    expect(fromSettings(s)).toMatchObject({ type: "bubble", x: "amount", xTitle: "Amount", y: "qty", size: "discount", sizeTitle: "Discount", series: "region" })
  })

  it("asks a scatter for numbers to place by, a bubble for its size too, each column once", () => {
    expect(missingField(form({ type: "scatter" }))).toBe("Pick the number column for the x axis.")
    expect(missingField(form({ type: "scatter", x: "a" }))).toBe("Pick the number column for the y axis.")
    expect(missingField(form({ type: "scatter", x: "a", y: "b" }))).toBeNull()
    expect(missingField(form({ type: "bubble", x: "a", y: "b" }))).toBe("Pick the number column each bubble's size reads.")
    expect(missingField(form({ type: "bubble", x: "a", y: "b", size: "a" }))).toBe("Pick a different column for each of x, y and size.")
  })

  it("sends a pivot its row and column fields and its measures in order, a later one only after an earlier", () => {
    const f = form({
      type: "pivot",
      pivotRows: [{ column: "region", title: "Region", grain: "" }, { column: "day", title: "", grain: "month" }],
      pivotColumns: [{ column: "channel", title: "", grain: "" }],
      aggregate: "sum",
      measureColumn: "amount",
      aggregate3: "count",
    })
    const s = toSettings(f)
    expect(s.roles).toEqual({
      rows: [{ column: "region", title: "Region" }, { column: "day", grain: "month" }],
      columns: [{ column: "channel" }],
      measure: [{ aggregate: "sum", column: "amount" }],
    })
    const back = fromSettings(toSettings({ ...f, aggregate2: "avg", measureColumn2: "amount" }))
    expect(back).toMatchObject({ pivotRows: f.pivotRows, pivotColumns: f.pivotColumns, aggregate2: "avg", aggregate3: "count", columns: [] })
  })

  it("asks a pivot for a row field, each field once, and measures that differ", () => {
    const base = { type: "pivot" as const, pivotRows: [{ column: "region", title: "", grain: "" as const }] }
    expect(missingField(form({ type: "pivot" }))).toBe("Pick at least one row field.")
    expect(missingField(form({ ...base }))).toBe("Pick what to measure.")
    expect(missingField(form({ ...base, pivotColumns: base.pivotRows, aggregate: "count" }))).toBe("A column is picked as a field twice; pick each once.")
    expect(missingField(form({ ...base, aggregate: "count", aggregate2: "sum" }))).toBe("Pick the column for measure 2.")
    expect(missingField(form({ ...base, aggregate: "count", aggregate2: "count" }))).toBe(
      "Two measures are the same; measure another column or use another aggregate.",
    )
    expect(missingField(form({ ...base, aggregate: "count", aggregate2: "avg", measureColumn2: "amount" }))).toBeNull()
  })
})

describe("savedChartView: the D4c types", () => {
  const echart = (v: ReturnType<typeof savedChartView>) => {
    if (v.kind !== "echart") throw new Error(`a ${v.kind}`)
    return v
  }
  const table = (v: ReturnType<typeof savedChartView>) => {
    if (v.kind !== "table") throw new Error(`a ${v.kind}`)
    return v
  }
  const POINTS: [string, string][] = [
    ["a", "the x"],
    ["b", "the y"],
  ]

  it("draws a scatter's points as one series named by its y, and none for a row with no x or y", () => {
    const v = echart(savedChartView(v2("scatter", { x: [{ column: "a" }], y: [{ column: "b", title: "B" }] }), result(POINTS, [["1", "2"], [null, "3"], ["4", null], [5, "6.5"]])))
    expect(v.settings).toMatchObject({
      type: "scatter",
      description: "B against a",
      points: { x: { field: "x", label: "a" }, y: { field: "y", label: "B" } },
      series: { values: [{ key: "", label: "B" }] },
    })
    expect("size" in (v.settings as { points: object }).points).toBe(false)
    expect(v.rows).toEqual([
      { x: 1, y: 2 },
      { x: 5, y: 6.5 },
    ])
  })

  it("draws a bubble largest first, so a small one is never hidden, and says how many had no size above zero", () => {
    const bubble = v2("bubble", { x: [{ column: "a" }], y: [{ column: "b" }], size: [{ column: "z" }] })
    const v = echart(savedChartView(bubble, result([...POINTS, ["z", ROLE_SIZE]], [[1, 1, 2], [2, 2, 9], [3, 3, 0], [4, 4, -1], [5, 5, null]])))
    expect(v.rows).toEqual([
      { x: 2, y: 2, size: 9 },
      { x: 1, y: 1, size: 2 },
    ])
    expect(v.notes).toEqual(["3 rows have no size above zero, so they are not drawn."])
    expect("rows" in (v.settings as { points: object }).points).toBe(false)
  })

  it("draws every point a capped bubble result sends, and never gives the rows with no size it read as the source's", () => {
    const bubble = v2("bubble", { x: [{ column: "a" }], y: [{ column: "b" }], size: [{ column: "z" }] })
    const cols: [string, string][] = [...POINTS, ["z", ROLE_SIZE]]
    // The server keeps only sized points under the cap: each is drawn, with no note on sizes.
    const sized = echart(savedChartView(bubble, { ...result(cols, [[1, 1, 2], [2, 2, 9]]), truncated: true }))
    expect(sized.rows).toHaveLength(2)
    expect(sized.notes).toEqual([])
    // A result that holds unsized rows counts only those it read: past the cap there may be more.
    const read = (rows: unknown[][]) => echart(savedChartView(bubble, { ...result(cols, rows), truncated: true })).notes
    expect(read([[1, 1, 2], [3, 3, 0], [4, 4, null]])).toEqual(["2 rows have no size above zero, so they are not drawn. Past the cap, the source may have more."])
    expect(read([[1, 1, 2], [3, 3, 0]])).toEqual(["1 row has no size above zero, so it is not drawn. Past the cap, the source may have more."])
  })

  it("reads each point's row count: the tooltip's field, the rows with no size, and the colour a pin goes to", () => {
    const bubble = v2("bubble", { x: [{ column: "a" }], y: [{ column: "b" }], size: [{ column: "z" }] })
    const counted: [string, string][] = [...POINTS, ["z", ROLE_SIZE], ["rsync_rows", ROLE_POINT_ROWS]]
    const v = echart(savedChartView(bubble, result(counted, [[1, 1, 2, 20000], [2, 2, 9, 1], [3, 3, 0, 1200], [4, 4, -1, 3]])))
    expect(v.settings).toMatchObject({ points: { rows: { field: "n", label: "Rows", format: "integer" } } })
    expect(v.rows).toEqual([
      { x: 2, y: 2, size: 9, n: 1 },
      { x: 1, y: 1, size: 2, n: 20000 },
    ])
    expect(v.notes).toEqual(["1,203 rows have no size above zero, so they are not drawn."])
    // Two colours pinned to one token: the one with more rows keeps it, as the server ranks them by rows.
    const pins = [{ value: "a", color: "series-4" }, { value: "b", color: "series-4" }]
    const coloured = v2("scatter", { x: [{ column: "a" }], y: [{ column: "b" }], series: [{ column: "c", colors: pins }] })
    const columns: [string, string][] = [...POINTS, ["c", "the series"], ["rsync_other", ROLE_OTHER], ["rsync_series", ROLE_SERIES_COUNT], ["rsync_rows", ROLE_POINT_ROWS]]
    const rows = [[1, 1, "a", 0, 9, 1], [2, 2, "a", 0, 9, 1], [3, 3, "a", 0, 9, 1], [4, 4, "b", 0, 9, 10]]
    const values = (echart(savedChartView(coloured, result(columns, rows))).settings as { series: { values: { label: string; color: string }[] } }).series.values
    expect(values.find((x) => x.label === "b")?.color).toBe("series-4")
    expect(values.find((x) => x.label === "a")?.color).not.toBe("series-4")
  })

  it("draws the 7 colours the gateway kept and Other, and says how many colours Other holds", () => {
    const coloured = v2("scatter", { x: [{ column: "a" }], y: [{ column: "b" }], series: [{ column: "c", colors: [{ value: "h", color: "series-4" }] }] })
    const columns: [string, string][] = [...POINTS, ["c", "the series"], ["rsync_other", ROLE_OTHER], ["rsync_series", ROLE_SERIES_COUNT]]
    // Seven kept colours, "a" with the most rows, and two folded into Other (series count 9).
    const kept = ["a", "a", "a", "b", "c", "d", "e", "f", "h"].map((c, i) => [i, i, c, 0, 9])
    const v = echart(savedChartView(coloured, result(columns, [...kept, [20, 20, null, 1, 9], [21, 21, null, 1, 9]])))
    const values = (v.settings as { series: { values: { key: string; label: string; color: string }[] } }).series.values
    expect(values.map((x) => x.label)).toEqual(["a", "b", "c", "d", "e", "f", "h", "Other"])
    expect(values.find((x) => x.label === "h")?.color).toBe("series-4")
    expect(values.at(-1)).toMatchObject({ key: "other", color: "other" })
    expect(v.rows.filter((r) => (r as { s: string }).s === "other")).toHaveLength(2)
    expect(v.notes).toEqual(["2 more colours are drawn as Other."])
  })

  it("draws no scatter when a column it places points by is masked", () => {
    const scatter = v2("scatter", { x: [{ column: "a" }], y: [{ column: "b" }] })
    expect(savedChartView(scatter, result(POINTS, [["***", 1]], ["a"]))).toEqual({
      kind: "message",
      message: "A column this chart places its points by is masked for you, so it is not drawn.",
    })
    expect(savedChartView(scatter, result([["a", "the x"]], [[1]]))).toMatchObject({ kind: "message" })
  })

  const pivot = v2("pivot", {
    rows: [{ column: "region" }],
    columns: [{ column: "channel" }],
    measure: [{ aggregate: "sum", column: "amount" }, { aggregate: "avg", column: "amount", format: "decimal" }],
  })
  const PIVOT: [string, string][] = [
    ["region", pivotRole("row", 1)],
    ["channel", pivotRole("column", 1)],
    ["amount", "the measure"],
    ["amount_2", ROLE_SECOND_MEASURE],
    ["t_r1", pivotTotalRole("row", 1)],
    ["t_c1", pivotTotalRole("column", 1)],
  ]
  // As ClickHouse sends GROUPING SETS: a totalled field holds its type's default, "" here,
  // not NULL, so only the flag tells a total from a value. EU has no web orders.
  const ROWS = [
    ["", "", "900", "112.5", 1, 1],
    ["", "store", "400", "100", 1, 0],
    ["", "web", "500", "125", 1, 0],
    ["EU", "", "300", "100", 0, 1],
    ["US", "", "600", "120", 0, 1],
    ["EU", "store", "300", "100", 0, 0],
    ["US", "store", "100", "100", 0, 0],
    ["US", "web", "500", "125", 0, 0],
  ]

  it("lays a pivot out as a cross-tab with the total row and column the source's own, by the flags", () => {
    const v = table(savedChartView(pivot, result(PIVOT, ROWS)))
    expect(v.head).toEqual([
      "region",
      "store · Sum of amount",
      "store · Average amount",
      "web · Sum of amount",
      "web · Average amount",
      "Total · Sum of amount",
      "Total · Average amount",
    ])
    expect(v.body).toEqual([
      ["EU", "300", "100", "", "", "300", "100"],
      ["US", "100", "100", "500", "125", "600", "120"],
      // 900 / 8 orders: the average of every order, not of the two regions' averages.
      ["Total", "400", "100", "500", "125", "900", "112.5"],
    ])
    expect(v.totals).toEqual([false, false, true])
    expect(v.numeric).toEqual([false, true, true, true, true, true, true])
    expect(v.notes).toEqual([])
  })

  it("keeps a region named like a total's default apart from the total, by the flag", () => {
    const rows = [["", "", "10", "10", 1, 1], ["", "", "4", "4", 0, 1], ["", "store", "4", "4", 0, 0]]
    const v = table(savedChartView(pivot, result(PIVOT, rows)))
    expect(v.body.map((r) => r[0])).toEqual(["", "Total"])
    expect(v.totals).toEqual([false, true])
  })

  it("leaves a masked measure as the server sent it, says a cut leaves the totals whole, and exports plain numbers", () => {
    const masked = table(savedChartView(pivot, { ...result(PIVOT, ROWS, ["amount"]), truncated: true }))
    expect(masked.body[2].slice(5)).toEqual(["900", "112.5"])
    expect(masked.notes).toEqual(["Each total counts every row of the source; it is cells past the cap that are not shown."])
    const big = ROWS.map((r) => r.map((c, i) => (i === 2 && c === "900" ? "1234567.5" : c)))
    expect(tableExport(pivot, result(PIVOT, big)).body[2]).toEqual(["Total", "400", "100", "500", "125", "1234567.5", "112.5"])
  })

  it("says no cells are shown, and a total may be missing, when the cap falls among the totals", () => {
    // The totals come first: a cap of four keeps the whole's, the two channels' and EU's, and cuts US's and every cell.
    const cut = table(savedChartView(pivot, { ...result(PIVOT, ROWS.slice(0, 4)), truncated: true }))
    expect(cut.body.map((r) => r[0])).toEqual(["EU", "Total"])
    // EU's cells are past the cap: marked, not left blank like EU's web orders, which it has none of.
    expect(cut.body[0]).toEqual(["EU", PAST, PAST, PAST, PAST, "300", "100"])
    expect(cut.notes).toEqual([
      "The totals alone reach the cap, so no cells are shown and a total may be missing. Each total shown counts every row of the source.",
      PAST_NOTE,
    ])
  })

  // A value past the row cap printed blank, as one the source has no rows for: the page
  // could not tell them apart. The server orders the rows by their total flags, so each
  // grouping set before the cut's is whole and each after it is past the cap; within the
  // cut's set, the rows before it are the ones it came with.
  const PAST = "…"
  const PAST_NOTE = "A … is past the cap and not shown; a blank has no value."
  // EU has no store orders and US has both channels.
  const SPARSE = [
    ["", "", "100", "25", 1, 1],
    ["", "store", "30", "30", 1, 0],
    ["", "web", "70", "23", 1, 0],
    ["EU", "", "40", "40", 0, 1],
    ["US", "", "60", "20", 0, 1],
    ["EU", "web", "40", "40", 0, 0],
    ["US", "store", "30", "30", 0, 0],
    ["US", "web", "30", "15", 0, 0],
  ]

  it("marks a cell past the cap, and leaves blank one before the cut that has no rows", () => {
    // The cut after EU's web: EU's store comes before it, so EU has none; US's come after.
    const six = table(savedChartView(pivot, { ...result(PIVOT, SPARSE.slice(0, 6)), truncated: true }))
    expect(six.body).toEqual([
      ["EU", "", "", "40", "40", "40", "40"],
      ["US", PAST, PAST, PAST, PAST, "60", "20"],
      ["Total", "30", "30", "70", "23", "100", "25"],
    ])
    // The cut after US's store: US's web comes after it, EU's store before it.
    const seven = table(savedChartView(pivot, { ...result(PIVOT, SPARSE.slice(0, 7)), truncated: true }))
    expect(seven.body.slice(0, 2)).toEqual([
      ["EU", "", "", "40", "40", "40", "40"],
      ["US", "30", "30", PAST, PAST, "60", "20"],
    ])
    expect(seven.notes).toEqual([
      "Each total counts every row of the source; it is cells past the cap that are not shown.",
      PAST_NOTE,
    ])
    // Uncut, or cut where nothing shown is past it, a blank is only a value the source has not.
    expect(table(savedChartView(pivot, result(PIVOT, SPARSE))).body[0]).toEqual(["EU", "", "", "40", "40", "40", "40"])
    const whole = table(savedChartView(pivot, { ...result(PIVOT, SPARSE), truncated: true }))
    expect(whole.body.flat()).not.toContain(PAST)
    expect(whole.notes).toEqual(["Each total counts every row of the source; it is cells past the cap that are not shown."])
    // A download carries the same mark, so a blank in it still means no value.
    expect(tableExport(pivot, { ...result(PIVOT, SPARSE.slice(0, 6)), truncated: true }).body[1]).toEqual(["US", PAST, PAST, PAST, PAST, "60", "20"])
  })

  it("marks a subtotal past the cap, where the cut falls among the column fields' totals", () => {
    const deep = v2("pivot", {
      rows: [{ column: "region" }],
      columns: [{ column: "channel" }, { column: "device" }],
      measure: [{ aggregate: "sum", column: "amount" }],
    })
    const DEEP: [string, string][] = [
      ["region", pivotRole("row", 1)],
      ["channel", pivotRole("column", 1)],
      ["device", pivotRole("column", 2)],
      ["amount", "the measure"],
      ["t_r1", pivotTotalRole("row", 1)],
      ["t_c1", pivotTotalRole("column", 1)],
      ["t_c2", pivotTotalRole("column", 2)],
    ]
    // The sets in the server's order: the whole, each channel, each region, each channel
    // and device; the cap cuts there, before each region's channels and devices.
    const rows = [
      ["", "", "", "150", 1, 1, 1],
      ["", "store", "", "70", 1, 0, 1],
      ["", "web", "", "80", 1, 0, 1],
      ["EU", "", "", "60", 0, 1, 1],
      ["US", "", "", "90", 0, 1, 1],
      ["", "store", "app", "50", 1, 0, 0],
      ["", "store", "desk", "20", 1, 0, 0],
    ]
    const v = table(savedChartView(deep, { ...result(DEEP, rows), truncated: true }))
    expect(v.head).toEqual([
      "region",
      "store · app · Sum of amount",
      "store · desk · Sum of amount",
      "store · Total · Sum of amount",
      "Total · Sum of amount",
    ])
    // EU's and US's store totals come after the cut: marked, never blank.
    expect(v.body).toEqual([
      ["EU", PAST, PAST, PAST, "60"],
      ["US", PAST, PAST, PAST, "90"],
      ["Total", "50", "20", "70", "150"],
    ])
  })

  it("says so when the result is missing a column the pivot needs", () => {
    expect(savedChartView(pivot, result(PIVOT.slice(0, 5), ROWS))).toEqual({
      kind: "message",
      message: "The result is missing a column the chart needs.",
    })
  })

  it("carries the description its author wrote on a scorecard, a table and a pivot, as a plot's settings do", () => {
    // The builder offers "Description (read to a screen reader)" for every type.
    const DESC = "Orders by region and channel"
    const card = v2("scorecard", { measure: [{ aggregate: "count" }] }, { description: ` ${DESC} ` })
    expect(savedChartView(card, result([["count", "the measure"]], [[3]]))).toMatchObject({ kind: "scorecard", description: DESC })
    const list = v2("table", { columns: [{ column: "region" }] }, { description: DESC })
    expect(savedChartView(list, result([["region", "table column region"]], [["EU"]]))).toMatchObject({ kind: "table", description: DESC })
    expect(savedChartView({ ...pivot, description: DESC }, result(PIVOT, ROWS))).toMatchObject({ kind: "table", description: DESC })
    // None written: nothing is read but the chart itself.
    expect(savedChartView({ ...pivot, description: "  " }, result(PIVOT, ROWS))).not.toHaveProperty("description")
  })
})

describe("the D4d types' fields", () => {
  const level = (column: string, title = "") => ({ column, title, grain: "" as const })

  it("offers every type the registry has, under the registry's name, and an x only to a type with one", () => {
    const offered = TYPE_GROUPS.flatMap((g) => g.types)
    expect(offered.map((t) => t.type).sort()).toEqual(CHART_REGISTRY.filter((t) => !t.deprecated).map((t) => t.type).sort())
    for (const t of offered) expect(t.label).toBe(CHART_REGISTRY.find((r) => r.type === t.type)?.label)
    expect(["sankey", "calendar", "boxplot", "candlestick", "range"].every((t) => takesX(t as ChartForm["type"]))).toBe(true)
    expect(["treemap", "sunburst", "bullet", "scatter", "pie"].some((t) => takesX(t as ChartForm["type"]))).toBe(false)
  })

  it("sends a treemap its levels, outermost first and three at most, and reads them back", () => {
    const f = form({
      type: "treemap",
      levels: [level("region", "Region"), level("country"), level("city"), level("street")],
      aggregate: "sum",
      measureColumn: "amount",
    })
    const s = toSettings(f)
    expect(s.roles).toEqual({
      rows: [{ column: "region", title: "Region" }, { column: "country" }, { column: "city" }],
      measure: [{ aggregate: "sum", column: "amount" }],
    })
    expect(fromSettings(s)).toMatchObject({ type: "treemap", levels: f.levels.slice(0, 3), pivotRows: [], aggregate: "sum" })
    // A level is grouped by its values: a grain left from a pivot is not sent.
    expect(toSettings(form({ type: "sunburst", levels: [{ column: "day", title: "", grain: "month" }], aggregate: "count" })).roles.rows).toEqual([
      { column: "day" },
    ])
  })

  it("asks a treemap for a level, each once, and a measure that adds up", () => {
    const one = { type: "treemap" as const, levels: [level("region")] }
    expect(missingField(form({ type: "treemap" }))).toBe("Pick at least one level.")
    expect(missingField(form({ ...one, levels: [level("region"), level("region")], aggregate: "count" }))).toBe(
      "A column is picked as a level twice; pick each once.",
    )
    expect(missingField(form({ ...one }))).toBe("Pick what to measure.")
    expect(missingField(form({ ...one, aggregate: "avg", measureColumn: "amount" }))).toBe("Each part is a share of a whole: measure a count or a sum.")
    expect(missingField(form({ ...one, aggregate: "sum" }))).toBe("Pick the column to measure.")
    expect(missingField(form({ ...one, type: "sunburst", aggregate: "count" }))).toBeNull()
  })

  it("sends a sankey where its flows start and end, with no grain, and asks for two columns", () => {
    const f = form({ type: "sankey", x: "from", xGrain: "day", xTitle: "From", y: "to", yGrain: "month", aggregate: "count", sort: "value_desc" })
    expect(toSettings(f)).toMatchObject({ roles: { x: [{ column: "from", title: "From" }], y: [{ column: "to" }], measure: [{ aggregate: "count" }] } })
    expect(toSettings(f).sort).toBeUndefined()
    expect(fromSettings(toSettings(f))).toMatchObject({ x: "from", y: "to", xGrain: "", yGrain: "" })
    expect(missingField(form({ type: "sankey" }))).toBe("Pick the column the flows start from.")
    expect(missingField(form({ type: "sankey", x: "a" }))).toBe("Pick the column the flows end at.")
    expect(missingField(form({ type: "sankey", x: "a", y: "a", aggregate: "count" }))).toBe(
      "Pick a different column for where the flows start and where they end.",
    )
    expect(missingField(form({ type: "sankey", x: "a", y: "b", aggregate: "max", measureColumn: "n" }))).toBe(
      "Each flow is as wide as its share: measure a count or a sum.",
    )
    expect(missingField(form({ type: "sankey", x: "a", y: "b", aggregate: "count" }))).toBeNull()
  })

  it("sends a calendar its days with no grain and no sort, and asks for the date column", () => {
    const f = form({ type: "calendar", x: "day", xGrain: "week", aggregate: "avg", measureColumn: "amount", sort: "x_asc", color: "series-3" })
    expect(toSettings(f)).toEqual({ version: 2, type: "calendar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "avg", column: "amount" }] } })
    expect(missingField(form({ type: "calendar" }))).toBe("Pick the date column for the days.")
    expect(missingField(form({ type: "calendar", x: "day" }))).toBe("Pick what to measure.")
    expect(missingField(form({ type: "calendar", x: "day", aggregate: "count" }))).toBeNull()
  })

  it("sends a bullet its target as a typed number or a second measure, never both", () => {
    const base = { type: "bullet" as const, aggregate: "sum" as const, measureColumn: "amount", color: "series-2" as const }
    const typed = toSettings(form({ ...base, target: -40, aggregate2: "count" }))
    expect(typed.roles).toEqual({ measure: [{ aggregate: "sum", column: "amount", color: "series-2" }] })
    expect(typed.target).toBe(-40)
    expect(toSettings(form({ ...base, target: 0 })).target).toBe(0)
    const measured = toSettings(form({ ...base, bulletTarget: "measure", target: 500, aggregate2: "max", measureColumn2: "goal" }))
    expect(measured.roles.measure).toEqual([{ aggregate: "sum", column: "amount", color: "series-2" }, { aggregate: "max", column: "goal" }])
    expect(measured.target).toBeUndefined()
    expect(fromSettings(typed)).toMatchObject({ bulletTarget: "number", target: -40, aggregate2: "" })
    expect(fromSettings(measured)).toMatchObject({ bulletTarget: "measure", target: null, aggregate2: "max", measureColumn2: "goal" })
  })

  it("asks a bullet for its measure and then its target", () => {
    const base = { type: "bullet" as const, aggregate: "count" as const }
    expect(missingField(form({ type: "bullet" }))).toBe("Pick what to measure.")
    expect(missingField(form({ ...base }))).toBe("Type the target, or measure it.")
    expect(missingField(form({ ...base, target: 0 }))).toBeNull()
    expect(missingField(form({ ...base, bulletTarget: "measure" }))).toBe("Pick what the target measures.")
    expect(missingField(form({ ...base, bulletTarget: "measure", aggregate2: "sum" }))).toBe("Pick the column for the target.")
    expect(missingField(form({ ...base, bulletTarget: "measure", aggregate2: "count" }))).toBe(
      "The target is the measure itself; measure another column or use another aggregate.",
    )
    expect(missingField(form({ ...base, bulletTarget: "measure", aggregate2: "sum", measureColumn2: "goal" }))).toBeNull()
  })

  it("sends a box plot its groups and its number column with the column's format, and no measure", () => {
    const f = form({ type: "boxplot", x: "day", xGrain: "month", y: "amount", yTitle: "Amount", format: "compact", aggregate: "sum", sort: "value_desc" })
    const s = toSettings(f)
    expect(s.roles).toEqual({ x: [{ column: "day", grain: "month" }], y: [{ column: "amount", title: "Amount", format: "compact" }] })
    expect(s.sort).toBeUndefined()
    expect(toSettings({ ...f, sort: "x_desc" }).sort).toBe("x_desc")
    expect(fromSettings(s)).toMatchObject({ x: "day", xGrain: "month", y: "amount", yTitle: "Amount", format: "compact" })
    expect(missingField(form({ type: "boxplot" }))).toBe("Pick the column whose values are the groups.")
    expect(missingField(form({ type: "boxplot", x: "plan" }))).toBe("Pick the number column to draw the boxes of.")
    expect(missingField(form({ type: "boxplot", x: "plan", y: "amount" }))).toBeNull()
  })

  it("sends a candlestick its open, high, low and close in that order, only all four", () => {
    const f = form({ type: "candlestick", x: "day", xGrain: "week", prices: ["o", "h", "l", "c"], y: "left_over" })
    const s = toSettings(f)
    expect(s.roles).toEqual({ x: [{ column: "day", grain: "week" }], y: [{ column: "o" }, { column: "h" }, { column: "l" }, { column: "c" }] })
    expect(fromSettings(s)).toMatchObject({ prices: ["o", "h", "l", "c"], y: "" })
    expect(toSettings({ ...f, prices: ["o", "", "l", "c"] }).roles.y).toBeUndefined()
    // A price may repeat (a period of one trade opens and closes at once); the server aliases each.
    expect(missingField({ ...f, prices: ["p", "p", "p", "p"] })).toBeNull()
    expect(missingField(form({ type: "candlestick" }))).toBe("Pick the date column for the periods.")
    expect(missingField({ ...f, prices: ["o", "h", "", "c"] })).toBe("Pick the open, high, low and close columns.")
  })

  it("sends range bands a low and a high measure, and asks for both, different", () => {
    const f = form({ type: "range", x: "day", aggregate: "min", measureColumn: "amount", aggregate2: "max", measureColumn2: "amount", sort: "value_asc" })
    const s = toSettings(f)
    expect(s.roles).toEqual({
      x: [{ column: "day" }],
      measure: [
        { aggregate: "min", column: "amount" },
        { aggregate: "max", column: "amount" },
      ],
    })
    expect(s.sort).toBeUndefined()
    expect(fromSettings(s)).toMatchObject({ aggregate: "min", aggregate2: "max", measureColumn2: "amount" })
    const base = { type: "range" as const, x: "day" }
    expect(missingField(form({ type: "range" }))).toBe("Pick the x column.")
    expect(missingField(form({ ...base }))).toBe("Pick what the low measures.")
    expect(missingField(form({ ...base, aggregate: "min" }))).toBe("Pick the column for the low.")
    expect(missingField(form({ ...base, aggregate: "count" }))).toBe("Pick what the high measures.")
    expect(missingField(form({ ...base, aggregate: "count", aggregate2: "max" }))).toBe("Pick the column for the high.")
    expect(missingField(form({ ...base, aggregate: "count", aggregate2: "count" }))).toBe(
      "The low and the high are the same measure; measure another column or use another aggregate.",
    )
    expect(missingField(f)).toBeNull()
  })

  it("greys out a box plot on an engine with no exact percentile, as chart_sql.go refuses it", () => {
    expect(refusedTypes("postgresql")).toEqual({})
    expect(refusedTypes("redshift")).toEqual({})
    expect(refusedTypes("bigquery")).toEqual({})
    expect(refusedTypes("clickhouse")).toEqual({})
    expect(refusedTypes()).toEqual({})
    expect(refusedTypes("mysql").boxplot).toMatch(/MySQL and MariaDB do not have/)
    expect(refusedTypes("MariaDB").boxplot).toMatch(/MySQL and MariaDB do not have/)
    expect(refusedTypes("sqlserver").boxplot).toMatch(/SQL Server has only as a window function/)
    expect(refusedTypes("mssql").boxplot).toMatch(/SQL Server/)
    expect(Object.keys(refusedTypes("mysql"))).toEqual(["boxplot"])
  })
})

describe("savedChartView: the D4d types", () => {
  const echart = (v: ReturnType<typeof savedChartView>) => {
    if (v.kind !== "echart") throw new Error(`a ${v.kind}`)
    return v
  }
  const COUNT = [{ aggregate: "count" as const }]
  const SUM = [{ aggregate: "sum" as const, column: "amount" }]

  it("draws a treemap's parts with the largest 7 top-level groups coloured, and leaves out a part of zero or below", () => {
    const tree = v2("treemap", { rows: [{ column: "region", title: "Region" }, { column: "city" }], measure: SUM })
    const cols: [string, string][] = [
      ["region", levelRole(1)],
      ["city", levelRole(2)],
      ["amount", "the measure"],
    ]
    const names = ["a", "b", "c", "d", "e", "f", "g", "h"]
    const rows = [...names.map((n, i) => [n, `${n}1`, String(10 + i)]), ["a", "a2", "0"], ["b", "b2", null]]
    const v = echart(savedChartView(tree, result(cols, rows)))
    expect(v.settings).toMatchObject({ type: "treemap", description: "Sum of amount by Region and city", levels: ["Region", "city"] })
    const groups = (v.settings as { groups: { key: string }[] }).groups.map((g) => g.key)
    // The 7 largest by their total; "a", the least, takes Other's colour.
    expect(groups).toEqual(["h", "g", "f", "e", "d", "c", "b"])
    expect(v.rows).toHaveLength(8)
    expect(v.rows[0]).toEqual({ path: ["a", "a1"], value: 10 })
    expect(v.notes).toEqual(["2 parts are zero or below, so they are not drawn."])
    expect(savedChartView(tree, result(cols.slice(0, 2), rows))).toMatchObject({ kind: "message", message: "The result is missing a column the chart needs." })
  })

  it("marks a treemap or a sunburst the cap cut as partial, so its shares say they are of the drawn parts", () => {
    const cols: [string, string][] = [
      ["region", levelRole(1)],
      ["amount", "the measure"],
    ]
    const rows = [
      ["a", 30],
      ["b", 20],
    ]
    for (const type of ["treemap", "sunburst"] as const) {
      const parts = v2(type, { rows: [{ column: "region" }], measure: SUM })
      expect(echart(savedChartView(parts, { ...result(cols, rows), truncated: true })).settings).toMatchObject({ type, partial: true })
      expect(echart(savedChartView(parts, result(cols, rows))).settings).not.toHaveProperty("partial")
    }
  })

  it("draws a sankey's flows in order, leaving out a flow to itself, one that closes a loop and one of zero", () => {
    const sankey = v2("sankey", { x: [{ column: "from" }], y: [{ column: "to" }], measure: COUNT })
    const cols: [string, string][] = [
      ["from", "the x"],
      ["to", "the y"],
      ["n", "the measure"],
    ]
    const rows = [
      ["visit", "cart", 50],
      ["cart", "paid", 20],
      ["paid", "visit", 5],
      ["cart", "cart", 3],
      ["visit", "left", 0],
    ]
    const v = echart(savedChartView(sankey, result(cols, rows)))
    expect(v.rows).toEqual([
      { source: "visit", target: "cart", value: 50 },
      { source: "cart", target: "paid", value: 20 },
    ])
    expect((v.settings as { nodes: { key: string }[] }).nodes.map((n) => n.key)).toEqual(["visit", "cart", "paid"])
    expect(v.notes).toEqual([
      "1 flow is zero or below, so it is not drawn.",
      "1 flow goes from a name to itself, so it is not drawn.",
      "1 flow would close a loop, so it is not drawn: a sankey's flows only go forward.",
    ])
  })

  it("places at most 100 names in a sankey and says how many flows reach past them", () => {
    const sankey = v2("sankey", { x: [{ column: "from" }], y: [{ column: "to" }], measure: COUNT })
    const cols: [string, string][] = [
      ["from", "the x"],
      ["to", "the y"],
      ["n", "the measure"],
    ]
    // Each flow places two new names: 50 flows place 100, the next two place none.
    const rows = Array.from({ length: 52 }, (_, i) => [`s${i}`, `t${i}`, 100 - i])
    const v = echart(savedChartView(sankey, result(cols, rows)))
    expect((v.settings as { nodes: unknown[] }).nodes).toHaveLength(MAX_SANKEY_NODES)
    expect(v.rows).toHaveLength(50)
    expect(v.notes).toEqual(["2 flows reach past the 100 names a sankey places, so they are not drawn."])
  })

  it("draws a calendar from its first day to its last, a day a row, and nothing without a day", () => {
    const cal = v2("calendar", { x: [{ column: "day" }], measure: COUNT })
    const cols: [string, string][] = [
      ["day", "the x"],
      ["n", "the measure"],
    ]
    const v = echart(savedChartView(cal, result(cols, [["2026-03-02T00:00:00Z", 4], ["2026-01-05", "2"], [null, 1], ["2026-02-01", null]])))
    expect(v.settings).toMatchObject({ type: "calendar", range: ["2026-01-05", "2026-03-02"] })
    expect(v.rows).toEqual([
      { day: "2026-03-02", value: 4 },
      { day: "2026-01-05", value: 2 },
    ])
    expect(savedChartView(cal, result(cols, [[null, 1]]))).toEqual({ kind: "message", message: "The result has no day with a number to show." })
  })

  it("draws a calendar over its latest 366 days at most, and says how many earlier days it leaves out", () => {
    const cal = v2("calendar", { x: [{ column: "day" }], measure: COUNT })
    const cols: [string, string][] = [
      ["day", "the x"],
      ["n", "the measure"],
    ]
    // Monthly rows over years: the 366 rows the server sends can span far more than 366 days.
    const v = echart(savedChartView(cal, result(cols, [["2026-11-01", 4], ["2025-11-01", 3], ["2025-10-31", 2], ["2021-02-01", 1]])))
    expect(v.settings).toMatchObject({ type: "calendar", range: ["2025-11-01", "2026-11-01"] })
    expect(v.rows).toEqual([
      { day: "2026-11-01", value: 4 },
      { day: "2025-11-01", value: 3 },
    ])
    expect(v.notes).toEqual(["2 earlier days are not drawn: the calendar shows the latest 366 days."])
    // A leap year's 366 days fit whole.
    const leap = echart(savedChartView(cal, result(cols, [["2024-12-31", 1], ["2024-01-01", 1], ["2023-12-31", 1]])))
    expect(leap.settings).toMatchObject({ range: ["2024-01-01", "2024-12-31"] })
    expect(leap.notes).toEqual(["1 earlier day is not drawn: the calendar shows the latest 366 days."])
    // A cut result cannot count the days it left out; its own banner says the source has more.
    const cut = echart(savedChartView(cal, { ...result(cols, [["2026-11-01", 4], ["2021-02-01", 1]]), truncated: true }))
    expect(cut.rows).toEqual([{ day: "2026-11-01", value: 4 }])
    expect(cut.notes).toEqual([])
  })

  it("draws a bullet against its typed target or its second measure, named for it", () => {
    const typed = v2("bullet", { measure: SUM }, { target: 1000 })
    const v = echart(savedChartView(typed, result([["amount", "the measure"]], [["800"]])))
    expect(v.rows).toEqual([{ value: 800, target: 1000 }])
    expect(v.settings).toMatchObject({ description: "Sum of amount against Target", target: { label: "Target" } })
    const measured = v2("bullet", { measure: [...SUM, { aggregate: "max", column: "goal", title: "Goal" }] })
    const cols: [string, string][] = [
      ["amount", "the measure"],
      ["goal", ROLE_SECOND_MEASURE],
    ]
    expect(echart(savedChartView(measured, result(cols, [[800, 950]]))).rows).toEqual([{ value: 800, target: 950 }])
    expect(savedChartView(measured, result(cols, [[800, "***"]], ["goal"]))).toEqual({
      kind: "message",
      message: "This chart's measure is masked for you, so it is not drawn.",
    })
    expect(savedChartView(typed, result([["amount", "the measure"]], []))).toEqual({ kind: "message", message: "The result has no number to show." })
  })

  it("gives a bullet's measured target its own number format, and a typed target none", () => {
    const cols: [string, string][] = [
      ["amount", "the measure"],
      ["goal", ROLE_SECOND_MEASURE],
    ]
    const target = (second: ChartField) => (echart(savedChartView(v2("bullet", { measure: [...SUM, second] }), result(cols, [[800, 950]]))).settings as { target: object }).target
    expect(target({ aggregate: "max", column: "goal", format: "compact" })).toEqual({ label: "Highest goal", format: "compact" })
    expect(target({ aggregate: "count" })).toEqual({ label: "Count", format: "integer" })
    const typed = echart(savedChartView(v2("bullet", { measure: SUM }, { target: 1000 }), result([["amount", "the measure"]], [[800]])))
    expect((typed.settings as { target: object }).target).toEqual({ label: "Target" })
  })

  const BOX: [string, string][] = [["plan", "the x"], ...BOX_ROLES.map((r, i): [string, string] => [`b${i}`, r])]

  it("draws a box per group from the server's quartiles, and says when the engine estimates them", () => {
    const box = v2("boxplot", { x: [{ column: "plan" }], y: [{ column: "amount", format: "integer" }] })
    const rows = [
      ["pro", 1, 2, 3, 4, 5],
      ["free", "0", "0.5", "1", "1.5", "9"],
    ]
    const v = echart(savedChartView(box, result(BOX, rows)))
    expect(v.settings).toMatchObject({
      type: "boxplot",
      x: { label: "plan", categories: ["pro", "free"] },
      y: { label: "amount", format: "integer" },
    })
    expect(v.rows[1]).toEqual({ x: "free", min: 0, q1: 0.5, median: 1, q3: 1.5, max: 9 })
    expect(v.notes).toEqual([])
    expect(echart(savedChartView(box, { ...result(BOX, rows), approximate: true })).notes).toEqual([
      "The quartiles are approximate: this engine estimates them.",
    ])
    expect(savedChartView(box, result(BOX, rows, ["b2"]))).toEqual({ kind: "message", message: "A column this chart draws is masked for you, so it is not drawn." })
  })

  it("draws a candlestick's periods from its open, high, low and close, named for its columns", () => {
    const candle = v2("candlestick", { x: [{ column: "day", grain: "month" }], y: ["o", "h", "l", "c"].map((column) => ({ column })) })
    const cols: [string, string][] = [["day", "the x"], ...PRICE_ROLES.map((r, i): [string, string] => [["o", "h", "l", "c"][i], r])]
    const v = echart(savedChartView(candle, result(cols, [["2026-01-01", 10, 12, 9, 11]])))
    expect(v.settings).toMatchObject({ type: "candlestick", description: "o, h, l and c by day", x: { categories: ["2026-01"] } })
    expect(v.rows).toEqual([{ x: "2026-01", open: 10, high: 12, low: 9, close: 11 }])
    expect(savedChartView(candle, result(cols.slice(0, 4), []))).toMatchObject({ kind: "message", message: "The result is missing a column the chart needs." })
  })

  // The server leaves out empty prices and values, so a period or a group with one that is
  // not a number came from a column of another kind: the renderer cannot draw it, and says how many.
  it("counts the candles and the boxes it cannot draw because a price or a value is not a number", () => {
    const candle = v2("candlestick", { x: [{ column: "day", grain: "month" }], y: ["currency", "h", "l", "c"].map((column) => ({ column })) })
    const cols: [string, string][] = [["day", "the x"], ...PRICE_ROLES.map((r, i): [string, string] => [["o", "h", "l", "c"][i], r])]
    const notes = (rows: unknown[][]) => echart(savedChartView(candle, result(cols, rows))).notes
    expect(notes([["2026-01-01", 10, 12, 9, 11]])).toEqual([])
    expect(notes([["2026-01-01", "EUR", 12, 9, 11], ["2026-02-01", 10, 12, 9, 11]])).toEqual(["1 period has a price that is not a number, so it is not drawn."])
    expect(notes([["2026-01-01", "EUR", 12, 9, 11], ["2026-02-01", "2026-02-01", 12, 9, 11]])).toEqual([
      "2 periods have a price that is not a number, so they are not drawn.",
    ])
    const box = v2("boxplot", { x: [{ column: "plan" }], y: [{ column: "currency" }] })
    const boxNotes = (rows: unknown[][], approximate = false) => echart(savedChartView(box, { ...result(BOX, rows), approximate })).notes
    expect(boxNotes([["pro", "AUD", "EUR", "GBP", "USD", "USD"], ["free", 1, 2, 3, 4, 5]], true)).toEqual([
      "The quartiles are approximate: this engine estimates them.",
      "1 group has values that are not numbers, so its box is not drawn.",
    ])
    expect(boxNotes([["pro", "AUD", "EUR", "GBP", "USD", "USD"], ["free", "AUD", "AUD", "AUD", "AUD", "AUD"]])).toEqual([
      "2 groups have values that are not numbers, so their boxes are not drawn.",
    ])
  })

  it("names a candlestick's price column once when it gives more than one price", () => {
    const named = (prices: string[]) => {
      const candle = v2("candlestick", { x: [{ column: "day", grain: "week" }], y: prices.map((column) => ({ column })) })
      const cols: [string, string][] = [["day", "the x"], ...PRICE_ROLES.map((r, i): [string, string] => [`p${i}`, r])]
      return echart(savedChartView(candle, result(cols, [["2026-01-05", 10, 12, 9, 11]]))).settings.description
    }
    expect(named(["card_spend", "card_spend", "card_spend", "card_spend"])).toBe("card_spend by day")
    expect(named(["low", "high", "low", "high"])).toBe("low and high by day")
  })

  it("draws range bands from each x's low and high, a gap where either is missing", () => {
    const range = v2("range", { x: [{ column: "day" }], measure: [{ aggregate: "min", column: "amount" }, { aggregate: "max", column: "amount", title: "High" }] })
    const cols: [string, string][] = [
      ["day", "the x"],
      ["lo", "the measure"],
      ["hi", ROLE_SECOND_MEASURE],
    ]
    const v = echart(savedChartView(range, result(cols, [["a", 1, 4], ["b", null, 3]])))
    expect(v.settings).toMatchObject({ type: "range", description: "Lowest amount to High by day", high: { label: "High" } })
    expect(v.rows).toEqual([
      { x: "a", low: 1, high: 4 },
      { x: "b", low: null, high: 3 },
    ])
  })

  // A date x with no grain was a category, so a bar of it came largest first.
  it("draws a bar over dates with no grain oldest first, a row with no date last", () => {
    const cols: [string, string][] = [
      ["day", "the x"],
      ["n", "the measure"],
    ]
    const bar = (rows: unknown[][], more: object = {}, type: "bar" | "hbar" = "bar") =>
      echart(savedChartView(v2(type, { x: [{ column: "day" }], measure: [{ aggregate: "count" }] }, more), result(cols, rows)))
    const xs = (v: ReturnType<typeof bar>) => (v.settings as { x: { categories: string[] } }).x.categories
    // The server's largest first, as it sends a bar of one measure in the default order.
    const largest = [["2026-10-03", 9], [null, 7], ["2026-10-01", 5], ["2026-10-02T09:30:00Z", 4]]
    expect(xs(bar(largest))).toEqual(["2026-10-01", "2026-10-02T09:30:00Z", "2026-10-03", "(empty)"])
    expect(bar(largest).rows[0]).toEqual({ x: "2026-10-01", v: 5 })
    expect(xs(bar(largest, {}, "hbar"))[0]).toBe("2026-10-01")
    // An order asked for, a cut result, or words on the x: as the server sends them.
    expect(xs(bar(largest, { sort: "value_desc" }))[0]).toBe("2026-10-03")
    const cut = echart(savedChartView(v2("bar", { x: [{ column: "day" }], measure: [{ aggregate: "count" }] }), { ...result(cols, largest), truncated: true }))
    expect(xs(cut)[0]).toBe("2026-10-03")
    expect(xs(bar([["b", 9], ["a", 5]]))).toEqual(["b", "a"])
  })

  // The null fill of a line (K5) was not applied to a range, so its band joined across a gap.
  it("breaks range bands at a period with no rows, as a line does", () => {
    const measure: ChartField[] = [{ aggregate: "min", column: "amount" }, { aggregate: "max", column: "amount" }]
    const cols: [string, string][] = [
      ["day", "the x"],
      ["lo", "the measure"],
      ["hi", ROLE_SECOND_MEASURE],
    ]
    const xs = (x: object, rows: unknown[][], more: object = {}) =>
      echart(savedChartView(v2("range", { x: [{ column: "day", ...x }], measure }, more), result(cols, rows)))
    const daily = xs({}, [["2026-10-01", 1, 4], ["2026-10-02", 2, 5], ["2026-10-04", 1, 3]])
    expect(daily.settings).toMatchObject({ x: { categories: ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"] } })
    expect(daily.rows[2]).toEqual({ x: "2026-10-03", low: null, high: null })
    const monthly = xs({ grain: "month" }, [["2026-08-01T00:00:00Z", 1, 4], ["2026-10-01T00:00:00Z", 2, 5]])
    expect((monthly.settings as { x: { categories: string[] } }).x.categories).toHaveLength(3)
    expect(monthly.rows.map((r) => (r as { low: number | null }).low)).toEqual([1, null, 2])
    const newest = xs({}, [["2026-10-04", 1, 3], ["2026-10-02", 2, 5], ["2026-10-01", 1, 4]], { sort: "x_desc" })
    expect(newest.settings).toMatchObject({ x: { categories: ["2026-10-04", "2026-10-03", "2026-10-02", "2026-10-01"] } })
    // A cut result, or words on the x, are drawn as they come.
    const cut = echart(savedChartView(v2("range", { x: [{ column: "day" }], measure }), { ...result(cols, [["2026-10-01", 1, 4], ["2026-10-04", 1, 3]]), truncated: true }))
    expect(cut.settings).toMatchObject({ x: { categories: ["2026-10-01", "2026-10-04"] } })
    expect(xs({}, [["a", 1, 4], ["c", 1, 3]]).settings).toMatchObject({ x: { categories: ["a", "c"] } })
    // No rows with a grain picked threw, as a line's did.
    expect(xs({ grain: "day" }, []).settings).toMatchObject({ x: { categories: [] } })
  })

  it("says a cut treemap keeps its largest parts and a cut calendar its latest days", () => {
    expect(truncatedNote("treemap")).toBe("The largest 3,500 parts drawn: the source has more.")
    expect(truncatedNote("calendar")).toBe("The latest 366 days drawn: the source has more.")
    // The tie-break is the SQL's (chart_sql.go): among points with as many rows, the least x then y.
    expect(truncatedNote("scatter")).toBe("The 10,000 points with the most rows drawn, ties broken by the smallest x then y: the source has more.")
    expect(truncatedNote("bubble")).toBe("The 10,000 points with the most rows drawn, ties broken by the smallest x then y: the source has more.")
    expect(truncatedNote("bar")).toBe("First 10,000 rows drawn: the source has more.")
    expect(truncatedNote()).toBe("First 10,000 rows drawn: the source has more.")
  })
})

describe("savedChartView: the map", () => {
  const echart = (v: ReturnType<typeof savedChartView>) => {
    if (v.kind !== "echart") throw new Error(`a ${v.kind}`)
    return v
  }
  const cols: [string, string][] = [
    ["country", "the x"],
    ["amount", "the measure"],
  ]
  const world = v2("map", { x: [{ column: "country" }], measure: [{ aggregate: "sum", column: "amount", color: "series-3" }] }, { map: "world" })

  it("fills a shape a row by its ISO code, in any of its forms, and names the table's column for the shapes", () => {
    const v = echart(savedChartView(world, result(cols, [["FR", "1200.5"], ["usa", 90], [392, "7"]])))
    expect(v.settings).toEqual({
      type: "map",
      description: "Sum of amount by country",
      map: "world",
      region: { label: "Country" },
      value: { label: "Sum of amount", format: "decimal" },
      color: "series-3",
    })
    expect(v.rows).toEqual([
      { region: "FRA", value: 1200.5 },
      { region: "USA", value: 90 },
      { region: "JPN", value: 7 },
    ])
    expect(v.notes).toEqual([])
    const titled = v2("map", { x: [{ column: "country", title: "Market" }], measure: [{ aggregate: "count" }] }, { map: "world", description: "Where" })
    expect(echart(savedChartView(titled, result(cols, [["FR", 1]]))).settings).toMatchObject({ description: "Where", region: { label: "Market" } })
  })

  it("never reads a name, and says which codes it could not place, up to three", () => {
    const v = echart(savedChartView(world, result(cols, [["FR", 9], ["France", 8], ["XX", 7], ["Atlantis", 6], ["XX", 5], ["ZZ", 4]])))
    expect(v.rows).toEqual([{ region: "FRA", value: 9 }])
    expect(v.notes).toEqual(["5 rows name no country by its ISO code, so they are not drawn: France, XX, Atlantis and more."])
    expect(echart(savedChartView(world, result(cols, [["FR", 9], ["France", 8]]))).notes).toEqual([
      "1 row names no country by its ISO code, so it is not drawn: France.",
    ])
    // Three codes, each once: no "and more".
    expect(echart(savedChartView(world, result(cols, [["FR", 9], ["France", 8], ["XX", 7], ["XX", 6], ["Atlantis", 5]]))).notes).toEqual([
      "4 rows name no country by its ISO code, so they are not drawn: France, XX and Atlantis.",
    ])
  })

  it("draws a shape once, from the first row that names it: the rows come largest first", () => {
    const v = echart(savedChartView(world, result(cols, [["FR", 9], ["FRA", 8], ["250", 7], ["DE", 6]])))
    expect(v.rows).toEqual([
      { region: "FRA", value: 9 },
      { region: "DEU", value: 6 },
    ])
    expect(v.notes).toEqual(["2 rows name a country a row above them already fills, so they are not drawn."])
    expect(echart(savedChartView(world, result(cols, [["FR", 9], ["FRA", 8]]))).notes).toEqual([
      "1 row names a country a row above it already fills, so it is not drawn.",
    ])
  })

  it("says so when no row names a shape, or the measure is masked, rather than draw an empty map", () => {
    expect(savedChartView(world, result(cols, [["France", 9], ["FR", null]]))).toEqual({
      kind: "message",
      message: "No row names a country by its ISO code (FR, FRA or 250), so there is nothing to fill.",
    })
    expect(savedChartView(world, result(cols, [["FR", 9]], ["amount"]))).toMatchObject({ kind: "message", message: "This chart's measure is masked for you, so it is not drawn." })
    expect(savedChartView(world, result(cols.slice(0, 1), [["FR"]]))).toMatchObject({ kind: "message", message: "The result is missing a column the chart needs." })
  })

  it("reads a US state by US-CA or CA, never by its name, and calls the column State", () => {
    const states = v2("map", { x: [{ column: "state" }], measure: [{ aggregate: "count" }] }, { map: "us_states" })
    const v = echart(savedChartView(states, result([["state", "the x"], ["n", "the measure"]], [["CA", 3], ["US-NY", 2], ["Texas", 1]])))
    expect(v.settings).toMatchObject({ map: "us_states", region: { label: "State" } })
    expect(v.rows).toEqual([
      { region: "US-CA", value: 3 },
      { region: "US-NY", value: 2 },
    ])
    expect(v.notes).toEqual(["1 row names no US state by its ISO code, so it is not drawn: Texas."])
    expect(savedChartView(states, result([["state", "the x"], ["n", "the measure"]], [["FR", 1]]))).toMatchObject({
      message: "No row names a US state by its ISO code (US-CA or CA), so there is nothing to fill.",
    })
  })
})
