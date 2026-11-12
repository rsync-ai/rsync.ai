"use client"

/**
 * A dashboard's filters, in one row above its tiles (dashboards-design.md § 4.2): a
 * date range or a list of values per filter. The server binds each as a parameter
 * on every tile whose source has the column; a tile that has not says so.
 */

import { ChevronDown, Loader2, X } from "lucide-react"
import { useEffect, useRef, useState } from "react"

import { FilterChips, type FilterChip } from "@/components/kit/FilterChips"
import { Button } from "@/components/ui/button"
import {
  MAX_FILTER_VALUES,
  filterValues,
  type DashboardFilter,
  type DashboardFilterValue,
} from "@/lib/api/dashboards"

const inputClass =
  "h-8 rounded-md border border-zinc-200 bg-white px-2 text-sm text-zinc-900 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-100"

export interface FilterBarProps {
  dashboardId: string
  filters: DashboardFilter[]
  picks: Record<string, DashboardFilterValue>
  onChange: (id: string, value: DashboardFilterValue | null) => void
}

export function FilterBar({ dashboardId, filters, picks, onChange }: FilterBarProps) {
  if (filters.length === 0) return null
  const any = filters.some((f) => picks[f.id])
  return (
    <div role="group" aria-label="Filters" className="flex flex-wrap items-end gap-3">
      {filters.map((f) =>
        f.kind === "date_range" ? (
          <DateRangeFilter key={f.id} filter={f} value={picks[f.id]} onChange={(v) => onChange(f.id, v)} />
        ) : (
          <ValuesFilter key={f.id} dashboardId={dashboardId} filter={f} value={picks[f.id]} onChange={(v) => onChange(f.id, v)} />
        ),
      )}
      {any && (
        <Button type="button" variant="ghost" size="sm" onClick={() => filters.forEach((f) => onChange(f.id, null))}>
          Clear filters
        </Button>
      )}
    </div>
  )
}

type DayRange = "7" | "30" | "90" | "all" | "custom"

const DAY_RANGES: FilterChip<DayRange>[] = [
  { value: "7", label: "7d" },
  { value: "30", label: "30d" },
  { value: "90", label: "90d" },
  { value: "all", label: "All" },
  { value: "custom", label: "Custom" },
]

/** A day as the filter sends it, YYYY-MM-DD in the viewer's own calendar, `back` days before today. */
function dayBefore(back: number): string {
  const d = new Date()
  d.setDate(d.getDate() - back)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`
}

/** The last `n` days, today included: both ends are whole days. */
function lastDays(n: number): { from: string; to: string } {
  return { from: dayBefore(n - 1), to: dayBefore(0) }
}

/** Which chip a range is: none is All, the last 7, 30 or 90 days that chip, anything else Custom. */
function dayRangeOf(value?: DashboardFilterValue): DayRange {
  if (!value?.from && !value?.to) return "all"
  for (const n of [7, 30, 90]) {
    const r = lastDays(n)
    if (value.from === r.from && value.to === r.to) return String(n) as DayRange
  }
  return "custom"
}

/**
 * A date range: the last 7, 30 or 90 days in one click, All for no range, or Custom
 * for any two days. Each chip sets the same from/to the two day pickers do.
 */
function DateRangeFilter({
  filter,
  value,
  onChange,
}: {
  filter: DashboardFilter
  value?: DashboardFilterValue
  onChange: (v: DashboardFilterValue | null) => void
}) {
  const [customOpen, setCustomOpen] = useState(false)
  // A range cleared from outside (Clear filters) leaves Custom for All.
  const empty = !value?.from && !value?.to
  const [wasEmpty, setWasEmpty] = useState(empty)
  if (empty !== wasEmpty) {
    setWasEmpty(empty)
    if (empty) setCustomOpen(false)
  }
  const range = customOpen ? "custom" : dayRangeOf(value)
  const from = value?.from ?? ""
  const to = value?.to ?? ""
  const set = (next: { from: string; to: string }) => onChange(next.from || next.to ? next : null)
  return (
    <fieldset className="flex flex-col gap-1.5">
      <legend className="mb-1 text-xs font-medium text-zinc-600 dark:text-zinc-400">{filter.label}</legend>
      <FilterChips
        label={`${filter.label} range`}
        chips={DAY_RANGES}
        value={range}
        onChange={(next) => {
          setCustomOpen(next === "custom")
          if (next === "all") onChange(null)
          else if (next !== "custom") onChange(lastDays(Number(next)))
        }}
      />
      {range === "custom" && (
        <div className="flex items-center gap-1.5">
          <input
            type="date"
            aria-label={`${filter.label} from`}
            className={inputClass}
            value={from}
            max={to || undefined}
            onChange={(e) => set({ from: e.target.value, to })}
          />
          <span className="text-xs text-zinc-500 dark:text-zinc-400">to</span>
          <input
            type="date"
            aria-label={`${filter.label} to`}
            className={inputClass}
            value={to}
            min={from || undefined}
            onChange={(e) => set({ from, to: e.target.value })}
          />
        </div>
      )}
      {(from && !to) || (!from && to) ? (
        <span className="text-[11px] text-zinc-500 dark:text-zinc-400">Pick both days to filter.</span>
      ) : from && to && from > to ? (
        <span className="text-[11px] text-amber-700 dark:text-amber-300">The range ends before it starts.</span>
      ) : null}
    </fieldset>
  )
}

/**
 * A value list: the column's values, read live when it opens, to tick; a value can
 * also be typed, for a column whose values are not listed (masked, or more than
 * the list holds). What is ticked applies when the list closes.
 */
function ValuesFilter({
  dashboardId,
  filter,
  value,
  onChange,
}: {
  dashboardId: string
  filter: DashboardFilter
  value?: DashboardFilterValue
  onChange: (v: DashboardFilterValue | null) => void
}) {
  const [open, setOpen] = useState(false)
  const [choices, setChoices] = useState<{ values: string[]; truncated: boolean; error?: string } | null>(null)
  const [loading, setLoading] = useState(false)
  const [draft, setDraft] = useState<string[]>([])
  const [typed, setTyped] = useState("")
  const ref = useRef<HTMLDivElement>(null)
  const picked = value?.values ?? []

  const close = (commit: boolean) => {
    setOpen(false)
    if (!commit) return
    const same = draft.length === picked.length && draft.every((v) => picked.includes(v))
    if (!same) onChange(draft.length > 0 ? { values: draft } : null)
  }
  // The latest close, for the listeners below.
  const closeRef = useRef(close)
  useEffect(() => {
    closeRef.current = close
  })

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) closeRef.current(true)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") closeRef.current(false)
    }
    document.addEventListener("mousedown", onDown)
    document.addEventListener("keydown", onKey)
    return () => {
      document.removeEventListener("mousedown", onDown)
      document.removeEventListener("keydown", onKey)
    }
  }, [open])

  const toggleOpen = () => {
    if (open) {
      close(true)
      return
    }
    setDraft(picked)
    setTyped("")
    setOpen(true)
    if (choices === null && !loading) {
      setLoading(true)
      filterValues(dashboardId, filter.id).then(
        (got) => setChoices(got),
        (err: unknown) =>
          setChoices({ values: [], truncated: false, error: err instanceof Error ? err.message : "Could not list the values" }),
      ).finally(() => setLoading(false))
    }
  }

  const toggle = (v: string) => setDraft((d) => (d.includes(v) ? d.filter((x) => x !== v) : [...d, v]))
  const addTyped = () => {
    const v = typed.trim()
    if (!v || draft.includes(v) || draft.length >= MAX_FILTER_VALUES) return
    setDraft((d) => [...d, v])
    setTyped("")
  }

  const shown = [...(choices?.values ?? []), ...draft.filter((v) => !(choices?.values ?? []).includes(v))]
  const summary = picked.length === 0 ? "All" : picked.length === 1 ? picked[0] : `${picked.length} picked`

  return (
    <div ref={ref} className="relative flex flex-col gap-1">
      <span className="text-xs font-medium text-zinc-600 dark:text-zinc-400" id={`filter-${filter.id}-label`}>
        {filter.label}
      </span>
      <button
        type="button"
        aria-haspopup="true"
        aria-expanded={open}
        aria-labelledby={`filter-${filter.id}-label filter-${filter.id}-value`}
        onClick={toggleOpen}
        className={`${inputClass} flex min-w-[9rem] max-w-[16rem] items-center justify-between gap-2`}
      >
        <span id={`filter-${filter.id}-value`} className="truncate">
          {summary}
        </span>
        <ChevronDown className="h-3.5 w-3.5 shrink-0 text-zinc-500 dark:text-zinc-400" aria-hidden />
      </button>
      {open && (
        <div
          role="dialog"
          aria-label={`${filter.label} values`}
          className="absolute left-0 top-full z-30 mt-1 w-72 space-y-2 rounded-md border border-zinc-200 bg-white p-2 shadow-lg dark:border-zinc-800 dark:bg-zinc-950"
        >
          {loading && (
            <div role="status" className="flex items-center gap-2 px-1 py-2 text-xs text-zinc-500 dark:text-zinc-400">
              <Loader2 className="h-3.5 w-3.5 animate-spin" /> Reading the values…
            </div>
          )}
          {choices?.error && <p className="px-1 text-xs text-amber-700 dark:text-amber-300">{choices.error}</p>}
          {shown.length > 0 && (
            <ul className="max-h-60 space-y-0.5 overflow-auto">
              {shown.map((v) => (
                <li key={v}>
                  <label className="flex cursor-pointer items-center gap-2 rounded px-1 py-1 text-sm hover:bg-zinc-50 dark:hover:bg-zinc-900">
                    <input type="checkbox" checked={draft.includes(v)} onChange={() => toggle(v)} />
                    <span className="truncate">{v === "" ? "(empty)" : v}</span>
                  </label>
                </li>
              ))}
            </ul>
          )}
          {choices && !choices.error && choices.values.length === 0 && (
            <p className="px-1 text-xs text-zinc-500 dark:text-zinc-400">No values to list.</p>
          )}
          {choices?.truncated && (
            <p className="px-1 text-[11px] text-zinc-500 dark:text-zinc-400">Only the first values are listed; type any other.</p>
          )}
          <div className="flex gap-1.5">
            <input
              className={`${inputClass} min-w-0 flex-1`}
              placeholder="Type a value"
              aria-label={`Type a ${filter.label} value`}
              value={typed}
              maxLength={200}
              onChange={(e) => setTyped(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault()
                  addTyped()
                }
              }}
            />
            <Button type="button" size="sm" variant="outline" onClick={addTyped} disabled={!typed.trim()}>
              Add
            </Button>
          </div>
          <div className="flex items-center justify-between border-t border-zinc-100 pt-2 dark:border-zinc-800">
            <Button type="button" size="sm" variant="ghost" onClick={() => setDraft([])} disabled={draft.length === 0}>
              <X className="mr-1 h-3.5 w-3.5" /> Clear
            </Button>
            <Button type="button" size="sm" onClick={() => close(true)}>
              Apply
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
