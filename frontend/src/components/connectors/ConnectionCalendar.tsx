"use client"

/**
 * A connection's calendar for charts (dashboards-design.md § 14.3, decision 14): the day
 * its weeks start and the month its fiscal year starts. A chart on this connection's data
 * buckets a date by week, quarter or year from them; ISO Monday and January until someone
 * changes them. Each pick saves on its own (the gateway's UpdateConnection takes the two
 * fields alone, with no connection test).
 */

import { useState } from "react"
import { toast } from "sonner"

import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

const WEEKDAYS = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"]
const MONTHS = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
]

interface ConnectionCalendarValue {
  week_start: number
  fiscal_year_start_month: number
}

const selectClass =
  "h-8 rounded-md border border-zinc-200 bg-white px-2 text-sm text-zinc-900 disabled:opacity-50 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-100"

export function ConnectionCalendar({
  connectionId,
  value,
  onSaved,
}: {
  connectionId: string
  /** As the gateway read it; a gateway from before migration 147 sends neither. */
  value: Partial<ConnectionCalendarValue>
  onSaved: (v: ConnectionCalendarValue) => void
}) {
  const [saving, setSaving] = useState(false)
  const current: ConnectionCalendarValue = {
    week_start: value.week_start || 1,
    fiscal_year_start_month: value.fiscal_year_start_month || 1,
  }

  const save = async (change: Partial<ConnectionCalendarValue>) => {
    setSaving(true)
    try {
      const response = await authFetch(API_ENDPOINTS.CONNECTIONS.UPDATE(connectionId), {
        method: "PUT",
        body: JSON.stringify(change),
      })
      if (!response.ok) {
        const body = (await response.json().catch(() => ({}))) as { message?: string; error?: string }
        toast.error("The calendar was not saved", { description: body.message ?? body.error ?? `HTTP ${response.status}` })
        return
      }
      onSaved({ ...current, ...change })
      toast.success("Calendar saved", { description: "Charts on this connection bucket weeks, quarters and years by it." })
    } catch (err) {
      toast.error("The calendar was not saved", { description: err instanceof Error ? err.message : String(err) })
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2" data-testid="connection-calendar">
      <label className="flex items-center gap-2">
        <span className="text-zinc-500 dark:text-zinc-400">Weeks start on</span>
        <select
          className={selectClass}
          value={current.week_start}
          disabled={saving}
          onChange={(e) => void save({ week_start: Number(e.target.value) })}
        >
          {WEEKDAYS.map((d, i) => (
            <option key={d} value={i + 1}>
              {d}
            </option>
          ))}
        </select>
      </label>
      <label className="flex items-center gap-2">
        <span className="text-zinc-500 dark:text-zinc-400">Fiscal year starts in</span>
        <select
          className={selectClass}
          value={current.fiscal_year_start_month}
          disabled={saving}
          onChange={(e) => void save({ fiscal_year_start_month: Number(e.target.value) })}
        >
          {MONTHS.map((m, i) => (
            <option key={m} value={i + 1}>
              {m}
            </option>
          ))}
        </select>
      </label>
    </div>
  )
}
