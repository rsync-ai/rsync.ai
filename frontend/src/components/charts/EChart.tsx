"use client"

import { BarChart, HeatmapChart, LineChart } from "echarts/charts"
import { GridComponent, TooltipComponent, VisualMapPiecewiseComponent } from "echarts/components"
import { init, use as registerModules, type EChartsType } from "echarts/core"
import { SVGRenderer } from "echarts/renderers"
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react"

import { chartModules, registeredGroups } from "@/components/charts/chart-modules"
import { chartLegend, chartOption, chartTable, sizeNote } from "@/components/charts/chart-option"
import { CHART_HEIGHT, type ChartProps } from "@/components/charts/chart-settings"
import { readChartTheme, type ChartTheme } from "@/components/charts/chart-theme"

// What most chart types draw, so the chunk holds no more of ECharts than that; the
// rest load with the first chart that needs them (chart-modules.ts). A heatmap is drawn
// only through a visualMap, kept hidden (chart-option.ts).
registerModules([BarChart, LineChart, HeatmapChart, GridComponent, TooltipComponent, VisualMapPiecewiseComponent, SVGRenderer])

export { CHART_HEIGHT }

/** Calls back when the class on <html> changes, which is where next-themes puts `.dark`. */
function onHtmlClassChange(callback: () => void): () => void {
  const observer = new MutationObserver(callback)
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] })
  return () => observer.disconnect()
}

// A string, so React compares the theme by value: an object would be new on every read.
const themeSnapshot = () => JSON.stringify(readChartTheme())

const reducedMotion = () => typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches

/**
 * One chart. Import EChartNoSSR, never this file: ECharts is only in the chunk
 * that file loads, on a page that draws a chart. An error drawing it goes to the
 * ChartBoundary EChartNoSSR puts around it.
 */
export function EChart({ settings, rows, height = CHART_HEIGHT, onCategoryClick }: ChartProps) {
  const ref = useRef<HTMLDivElement>(null)
  const chartRef = useRef<EChartsType | null>(null)
  const clickRef = useRef(onCategoryClick)
  // The drawn settings as JSON: a page that polls builds equal settings anew
  // each time, and only a change in what they say should start the chart over.
  const drawnSettings = useRef<string | null>(null)
  // The theme the page has, not the one next-themes' state says: next-themes sets
  // `.dark` on <html> in its provider's effect, which React runs after this
  // component's, so a redraw on useTheme() read the previous theme's tokens.
  const themeJson = useSyncExternalStore(onHtmlClassChange, themeSnapshot)
  const theme = useMemo(() => JSON.parse(themeJson) as ChartTheme, [themeJson])
  // A type's modules that failed to load, thrown in render for the ChartBoundary.
  const [failed, setFailed] = useState<unknown>(null)
  if (failed) throw failed
  // Counts the charts made: one made before its type's modules registered is made again (chart-modules.ts).
  const [made, makeAgain] = useState(0)
  const madeWithGroups = useRef(0)

  const fill = height === "fill"
  // Filling, the chart is the size of its box: ECharts reads it, and again on a resize.
  const px = fill ? undefined : height
  // The size the chart is drawn at, to the pixel: a new width or height draws it again,
  // so its axis labels turn or thin to fit and its parts, gaps and names fit the height
  // (chart-option.ts).
  const [width, setWidth] = useState<number>()
  const [tallness, setTallness] = useState<number>()
  // A box 0 across or 0 high (a panel collapsed, a parent hidden) is neither resized nor
  // drawn into: ECharts lays a map out in it with a scale of 0, and the inverse of its
  // transform throws. Its size is forgotten, so the size it comes back at draws it again.
  const hidden = useRef(false)

  useEffect(() => {
    const el = ref.current!
    const chart = init(el, undefined, { renderer: "svg", height: px })
    chartRef.current = chart
    madeWithGroups.current = registeredGroups()
    // A new chart is drawn from nothing.
    drawnSettings.current = null
    const resize = new ResizeObserver(() => {
      const [across, tall] = [Math.round(el.clientWidth), Math.round(el.clientHeight)]
      hidden.current = !across || !tall
      if (!hidden.current) chart.resize()
      setWidth(hidden.current ? undefined : across)
      setTallness(hidden.current ? undefined : tall)
    })
    resize.observe(el)
    return () => {
      resize.disconnect()
      chart.dispose()
      chartRef.current = null
    }
    // Made again only for a type's modules: a new height resizes this chart (below) rather than make another.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [made])

  useEffect(() => {
    if (!hidden.current) chartRef.current?.resize(px === undefined ? undefined : { height: px })
  }, [px])

  // The latest callback, so a new function each render does not re-register the handler.
  useEffect(() => {
    clickRef.current = onCategoryClick
  })
  const clickable = onCategoryClick !== undefined
  useEffect(() => {
    if (!clickable) return
    const chart = chartRef.current!
    const onClick = (p: { dataIndex?: number }) => {
      if (typeof p.dataIndex === "number") clickRef.current?.(p.dataIndex)
    }
    chart.on("click", onClick)
    return () => {
      // Unmounting, the chart is disposed of first, with its handlers.
      if (!chart.isDisposed()) chart.off("click", onClick)
    }
  }, [clickable, made])

  useEffect(() => {
    let live = true
    // New settings start over. The same settings with new rows or a new theme
    // merge, so the marks move to their new values and colours instead of being
    // drawn again from nothing.
    const draw = () => {
      const chart = chartRef.current
      if (!live || !chart || hidden.current) return
      // Made before a group registered, the chart cannot lay out that group's types.
      if (madeWithGroups.current < registeredGroups()) {
        makeAgain((n) => n + 1)
        return
      }
      const json = JSON.stringify(settings)
      // Read here, as a chart's first draw comes before its first resize. The height is
      // read with it, for how many parts a treemap or a sunburst can line and name, and
      // how far apart a funnel's stages, a sankey's names and a heatmap's rows are.
      const across = Math.round(ref.current?.clientWidth ?? 0) || undefined
      const tall = Math.round(ref.current?.clientHeight ?? 0) || undefined
      chart.setOption({ ...chartOption(settings, rows, theme, across, tall), animation: !reducedMotion() }, { notMerge: drawnSettings.current !== json })
      drawnSettings.current = json
    }
    const modules = chartModules(settings.type)
    if (modules) {
      // Modules that fail to load, or a draw that throws once they have, go to the
      // ChartBoundary as a throw drawing at once does.
      modules.then(draw).catch((err: unknown) => {
        if (live) setFailed(err)
      })
    } else {
      draw()
    }
    return () => {
      live = false
    }
  }, [settings, rows, theme, made, width, tallness])

  const legend = useMemo(() => chartLegend(settings, theme, rows), [settings, theme, rows])
  const table = useMemo(() => chartTable(settings, rows), [settings, rows])
  // What the chart cannot draw at this size (parts under a pixel, bins added up) is said
  // under it, as the table holds it all. Only before the first measure is the default size read.
  const note = useMemo(() => sizeNote(settings, rows, theme, width, tallness), [settings, rows, theme, width, tallness])

  return (
    <div className={fill ? "flex min-h-0 w-full flex-1 flex-col" : "w-full"}>
      <div
        ref={ref}
        role="img"
        aria-label={settings.description}
        className={fill ? "min-h-0 w-full flex-1" : "w-full"}
        style={fill ? undefined : { height: px }}
        data-testid="echart"
      />
      {note && (
        <p className="mt-1 text-[11px] text-muted-foreground" data-testid="chart-size-note">
          {note}
        </p>
      )}
      {/* Hidden from a screen reader, which reads the table instead. Wraps, so it never overflows the card. */}
      {legend.length > 0 && (
        <ul aria-hidden="true" className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-muted-foreground" data-testid="chart-legend">
          {legend.map((item) => (
            <li key={item.label} className="flex items-center gap-1.5">
              <span className="inline-block h-2 w-2 shrink-0 rounded-[2px]" style={{ background: item.color }} />
              {item.label}
            </li>
          ))}
        </ul>
      )}
      {/* sr-only on a wrapper, not the table: a table ignores the 1px width and the
          overflow, so it kept its full width and pushed a phone's page sideways. */}
      <div className="sr-only">
        <table>
          <caption>{settings.description}</caption>
          <thead>
            <tr>
              {table.head.map((h) => (
                <th key={h} scope="col">
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {table.body.map((cells, row) => (
              <tr key={row}>
                {cells.map((cell, i) => (i === 0 ? <th key={i} scope="row">{cell}</th> : <td key={i}>{cell}</td>))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
