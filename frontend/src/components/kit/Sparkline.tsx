import { Fragment } from "react"

import { cn } from "@/lib/utils"

export type SparklineColor = "series-1" | "series-7" | "failed"

const W = 100
const H = 24

/**
 * A trend line with no axes, under a number. Plain SVG rather than ECharts: no
 * chunk to wait for, no layout jump, and the stroke follows the theme through
 * the chart tokens. Decorative; the number beside it carries the meaning.
 */
export function Sparkline({
  values,
  color = "series-1",
  className,
}: {
  /** Evenly spaced; null where a period has no value, which breaks the line. */
  values: readonly (number | null)[]
  /** A chart colour token from globals.css, without its "--chart-" prefix. */
  color?: SparklineColor
  className?: string
}) {
  const numbers = values.filter((v): v is number => v !== null)
  if (values.length < 2 || numbers.length === 0) return null
  // From 0, or from the lowest value when one is below it (a net sum can be).
  const min = Math.min(...numbers, 0)
  const max = Math.max(...numbers, min + 1)
  const step = W / (values.length - 1)
  // Each run of values between breaks is its own line and area; a lone value is a dot.
  const runs: (readonly [number, number])[][] = []
  values.forEach((v, i) => {
    if (v === null) return
    if (i === 0 || values[i - 1] === null) runs.push([])
    runs[runs.length - 1].push([i * step, H - 1 - ((v - min) / (max - min)) * (H - 2)])
  })
  const pointsOf = (run: (readonly [number, number])[]) => run.map(([x, y]) => `${x.toFixed(2)},${y.toFixed(2)}`).join(" ")
  const edge = (x: number) => String(Number(x.toFixed(2)))
  return (
    <svg
      aria-hidden
      data-testid="sparkline"
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      className={cn("h-6 w-full overflow-visible", className)}
    >
      {runs.map((run, k) => (
        <Fragment key={k}>
          <polygon points={`${edge(run[0][0])},${H} ${pointsOf(run)} ${edge(run[run.length - 1][0])},${H}`} style={{ fill: `hsl(var(--chart-${color}) / 0.12)` }} />
          <polyline
            points={run.length === 1 ? `${pointsOf(run)} ${pointsOf(run)}` : pointsOf(run)}
            fill="none"
            vectorEffect="non-scaling-stroke"
            strokeWidth={1.5}
            strokeLinejoin="round"
            strokeLinecap="round"
            style={{ stroke: `hsl(var(--chart-${color}))` }}
          />
        </Fragment>
      ))}
    </svg>
  )
}
