"use client"

import { useState } from "react"
import { Calendar, Check } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { cn } from "@/lib/utils"

/**
 * The Pipelines list's "Created" filter: one button that reads "Any time",
 * "Last 7 days" or a date range, instead of two bare native date inputs that
 * had no label and read as two empty boxes.
 */

const PRESET_DAYS = [7, 30, 90] as const

export type CreatedRange =
  | { kind: "any" }
  | { kind: "last"; days: (typeof PRESET_DAYS)[number] }
  /** Local calendar days, YYYY-MM-DD, either side may be empty. */
  | { kind: "custom"; from: string; to: string }

export const ANY_TIME: CreatedRange = { kind: "any" }

export function isAnyTime(r: CreatedRange): boolean {
  return r.kind === "any" || (r.kind === "custom" && !r.from && !r.to)
}

function localDay(day: string, endOfDay: boolean): Date {
  const [y, m, d] = day.split("-").map(Number)
  return endOfDay ? new Date(y, m - 1, d, 23, 59, 59, 999) : new Date(y, m - 1, d)
}

/**
 * The list's created_from / created_to params (both compared as timestamps by
 * pipelines.go ListPipelines). Each side is a full instant at the user's local
 * day boundary: a bare "2026-10-07" cast to midnight, so "to" left that whole day out.
 */
export function createdRangeParams(r: CreatedRange, now: Date = new Date()): { from?: string; to?: string } {
  if (r.kind === "last") {
    const start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - r.days)
    return { from: start.toISOString() }
  }
  if (r.kind === "custom") {
    return {
      from: r.from ? localDay(r.from, false).toISOString() : undefined,
      to: r.to ? localDay(r.to, true).toISOString() : undefined,
    }
  }
  return {}
}

function shortDay(day: string): string {
  return localDay(day, false).toLocaleDateString(undefined, { month: "short", day: "numeric" })
}

export function createdRangeLabel(r: CreatedRange): string {
  if (r.kind === "last") return `Last ${r.days} days`
  if (r.kind === "custom") {
    if (r.from && r.to) return `${shortDay(r.from)} – ${shortDay(r.to)}`
    if (r.from) return `Since ${shortDay(r.from)}`
    if (r.to) return `Until ${shortDay(r.to)}`
  }
  return "Any time"
}

const dateInputClass =
  "w-full rounded-md border border-zinc-200 bg-transparent px-2 py-1.5 text-sm text-zinc-900 dark:border-zinc-800 dark:text-zinc-100"

export function CreatedRangeFilter({
  value,
  onChange,
}: {
  value: CreatedRange
  onChange: (next: CreatedRange) => void
}) {
  const [open, setOpen] = useState(false)
  const custom = value.kind === "custom" ? value : { kind: "custom" as const, from: "", to: "" }

  const pick = (next: CreatedRange) => {
    onChange(next)
    setOpen(false)
  }

  const presets: Array<{ label: string; range: CreatedRange; on: boolean }> = [
    { label: "Any time", range: ANY_TIME, on: isAnyTime(value) },
    ...PRESET_DAYS.map((days) => ({
      label: `Last ${days} days`,
      range: { kind: "last", days } as CreatedRange,
      on: value.kind === "last" && value.days === days,
    })),
  ]

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className="w-full justify-start gap-2 font-normal @md:w-auto" aria-label={`Created: ${createdRangeLabel(value)}`}>
          <Calendar className="h-4 w-4 text-zinc-500 dark:text-zinc-400" />
          {createdRangeLabel(value)}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 space-y-3 p-2">
        <div role="group" aria-label="Created" className="space-y-0.5">
          {presets.map((p) => (
            <button
              key={p.label}
              type="button"
              aria-pressed={p.on}
              onClick={() => pick(p.range)}
              className={cn(
                "flex w-full items-center justify-between rounded px-2 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800",
                p.on && "font-medium text-zinc-900 dark:text-white",
              )}
            >
              {p.label}
              {p.on && <Check className="h-4 w-4" aria-hidden />}
            </button>
          ))}
        </div>
        <fieldset className="space-y-2 border-t border-zinc-200 px-2 pt-3 dark:border-zinc-800">
          <legend className="sr-only">Custom range</legend>
          <p className="text-xs font-medium text-zinc-500 dark:text-zinc-400" aria-hidden>
            Custom range
          </p>
          <label className="block space-y-1 text-xs text-zinc-500 dark:text-zinc-400">
            <span>From</span>
            <input
              type="date"
              value={custom.from}
              max={custom.to || undefined}
              onChange={(e) => onChange({ ...custom, from: e.target.value })}
              className={dateInputClass}
            />
          </label>
          <label className="block space-y-1 text-xs text-zinc-500 dark:text-zinc-400">
            <span>To</span>
            <input
              type="date"
              value={custom.to}
              min={custom.from || undefined}
              onChange={(e) => onChange({ ...custom, to: e.target.value })}
              className={dateInputClass}
            />
          </label>
        </fieldset>
      </PopoverContent>
    </Popover>
  )
}
