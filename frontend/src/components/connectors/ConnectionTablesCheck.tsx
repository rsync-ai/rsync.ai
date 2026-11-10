"use client"

/**
 * The tables an API connection could read when it was last checked
 * (generator-trust plan, phase 3b).
 *
 * A connection test checks one endpoint, the auth check. Saving an API connection,
 * or "Check tables" here, reads one page of every table (no row is kept) and the
 * gateway stores the answer on the connection (last_table_check, migration 142)
 * until the next check. A table that fails never blocks the save; it is shown here
 * so it is found before a pipeline's first run.
 */

import { useState } from "react"
import { AlertTriangle, CheckCircle2, MinusCircle, RefreshCw, Settings2, XCircle } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"

export interface TableCheckEntry {
  name: string
  status: "ok" | "failed" | "needs_setting" | "not_checked" | string
  error?: string
  missing_settings?: string[]
  warnings?: string[]
}

/** The stored record: the result, or `error` when the tables could not be listed. */
export interface TableCheckRecord {
  checked_at?: string
  error?: string
  tables?: TableCheckEntry[]
  ok?: number
  failed?: number
  needs_setting?: number
  not_checked?: number
}

/** What a create, an edit or "Check tables" answers about the check. */
export interface TableCheckAnswer {
  last_table_check?: TableCheckRecord | null
  table_check_pending?: boolean
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

/** "4 of 6 tables readable · 1 failed · 1 needs a setting", or why none was checked. */
export function tableCheckSummary(r: TableCheckRecord): string {
  if (r.error) return "The tables could not be listed"
  const ok = r.ok ?? 0
  const failed = r.failed ?? 0
  const needsSetting = r.needs_setting ?? 0
  const notChecked = r.not_checked ?? 0
  const total = r.tables?.length ?? ok + failed + needsSetting + notChecked
  if (total === 0) return "The connector lists no tables"
  const parts = [`${ok} of ${plural(total, "table", "tables")} readable`]
  if (failed) parts.push(`${failed} failed`)
  if (needsSetting) parts.push(`${needsSetting} ${needsSetting === 1 ? "needs a setting" : "need a setting"}`)
  if (notChecked) parts.push(`${notChecked} not checked`)
  return parts.join(" · ")
}

/** True when the record has anything the user should look at. */
export function tableCheckHasProblems(r: TableCheckRecord): boolean {
  return !!r.error || (r.failed ?? 0) > 0 || (r.needs_setting ?? 0) > 0 || (r.not_checked ?? 0) > 0
}

/**
 * The toast after a save (or a check) of an API connection: a warning when a table
 * failed, needs a setting or was not checked; a note when the check is still
 * running. Null when every table was read, or the answer carries no check.
 */
export function tableCheckSaveNotice(
  answer: TableCheckAnswer | null | undefined,
): { level: "warning" | "info"; title: string; description: string } | null {
  if (!answer) return null
  if (answer.table_check_pending) {
    return {
      level: "info",
      title: "Still checking the tables",
      description: "Each table's first page is being read. The result shows on the connection's page when it ends.",
    }
  }
  const r = answer.last_table_check
  if (!r || !tableCheckHasProblems(r)) return null
  if (r.error) {
    return { level: "warning", title: "Saved, but its tables could not be listed", description: r.error }
  }
  const failed = (r.tables ?? []).filter((t) => t.status === "failed").map((t) => t.name)
  const shown = failed.slice(0, 3).join(", ")
  const more = failed.length > 3 ? ` and ${failed.length - 3} more` : ""
  return {
    level: "warning",
    title: `Saved — ${tableCheckSummary(r)}`,
    description: failed.length
      ? `Could not read: ${shown}${more}. See Tables on the connection's page.`
      : "See Tables on the connection's page.",
  }
}

/** Shows tableCheckSaveNotice's toast, if any. */
export function notifyTableCheck(answer: TableCheckAnswer | null | undefined): void {
  const notice = tableCheckSaveNotice(answer)
  if (!notice) return
  if (notice.level === "warning") toast.warning(notice.title, { description: notice.description, duration: 12000 })
  else toast.info(notice.title, { description: notice.description })
}

function EntryRow({ entry }: { entry: TableCheckEntry }) {
  const icon =
    entry.status === "failed" ? (
      <XCircle className="h-4 w-4 text-red-600 dark:text-red-400 mt-0.5 flex-shrink-0" />
    ) : entry.status === "needs_setting" ? (
      <Settings2 className="h-4 w-4 text-amber-600 dark:text-amber-400 mt-0.5 flex-shrink-0" />
    ) : (
      <MinusCircle className="h-4 w-4 text-zinc-400 mt-0.5 flex-shrink-0" />
    )
  const detail =
    entry.status === "failed"
      ? entry.error || "The connector returned no error message."
      : entry.status === "needs_setting"
        ? `Needs a setting: ${(entry.missing_settings ?? []).join(", ") || "see the connector's settings"}`
        : "Not checked: the check ran out of time first."
  return (
    <li className="flex items-start gap-2 py-1.5" data-testid={`table-check-${entry.status}`}>
      {icon}
      <div className="min-w-0">
        <span className="font-mono text-sm text-zinc-900 dark:text-white">{entry.name}</span>
        <p className="break-words text-xs text-zinc-600 dark:text-zinc-400">{detail}</p>
      </div>
    </li>
  )
}

/** The connection page's Tables card, with "Check tables". */
export function ConnectionTablesCard({
  connectionId,
  record,
  onChecked,
}: {
  connectionId: string
  record: TableCheckRecord | null | undefined
  onChecked: (record: TableCheckRecord) => void
}) {
  const [checking, setChecking] = useState(false)

  const checkTables = async () => {
    try {
      setChecking(true)
      const response = await authFetch(API_ENDPOINTS.CONNECTIONS.CHECK_TABLES(connectionId), { method: "POST" })
      const body = (await response.json().catch(() => ({}))) as TableCheckAnswer & { message?: string; error?: string }
      if (response.status === 200 && body.last_table_check) {
        onChecked(body.last_table_check)
        if (!tableCheckHasProblems(body.last_table_check)) {
          toast.success("Every table was read", { description: tableCheckSummary(body.last_table_check) })
        }
        return
      }
      if (response.status === 202) {
        toast.info("Still checking the tables", {
          description: "The result is stored when the check ends. Reload this page in a minute to see it.",
        })
        return
      }
      toast.error("Could not check the tables", {
        description: body.message || body.error || `HTTP ${response.status}`,
      })
    } catch (err) {
      toast.error("Could not check the tables", { description: err instanceof Error ? err.message : String(err) })
    } finally {
      setChecking(false)
    }
  }

  const checkedAt = record?.checked_at ? formatAbsoluteTime(record.checked_at) : null
  const problems = (record?.tables ?? []).filter((t) => t.status !== "ok")
  const readable = (record?.tables ?? []).filter((t) => t.status === "ok").map((t) => t.name)

  return (
    <Card data-testid="connection-tables-card">
      <CardContent className="p-6 space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h3 className="text-sm font-medium text-zinc-900 dark:text-white">Tables</h3>
            <p className="text-xs text-zinc-500 dark:text-zinc-400 mt-1">
              Each table&apos;s first page is read when the connection is saved, or with Check tables. No row is kept.
            </p>
          </div>
          <Button variant="outline" size="sm" onClick={checkTables} disabled={checking}>
            <RefreshCw className={`h-3.5 w-3.5 mr-1.5 ${checking ? "animate-spin" : ""}`} />
            {checking ? "Checking..." : "Check tables"}
          </Button>
        </div>

        {!record ? (
          <p className="text-sm text-zinc-600 dark:text-zinc-400" data-testid="table-check-never">
            Not checked yet.
          </p>
        ) : (
          <>
            <p className="flex items-center gap-2 text-sm" data-testid="table-check-summary">
              {tableCheckHasProblems(record) ? (
                <AlertTriangle className="h-4 w-4 text-amber-600 dark:text-amber-400" />
              ) : (
                <CheckCircle2 className="h-4 w-4 text-green-600 dark:text-green-400" />
              )}
              <span className="font-medium text-zinc-900 dark:text-white">{tableCheckSummary(record)}</span>
              {checkedAt && (
                <span className="text-xs text-zinc-500 dark:text-zinc-400" title={checkedAt}>
                  · {formatRelativeTime(record.checked_at as string)}
                </span>
              )}
            </p>
            {record.error && (
              <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-red-50 p-2 font-mono text-xs text-red-900 dark:bg-red-950/40 dark:text-red-200">
                {record.error}
              </pre>
            )}
            {problems.length > 0 && (
              <ul className="divide-y divide-zinc-100 dark:divide-zinc-800">
                {problems.map((entry) => (
                  <EntryRow key={entry.name} entry={entry} />
                ))}
              </ul>
            )}
            {readable.length > 0 && (
              <p className="break-words text-xs text-zinc-500 dark:text-zinc-400">
                Readable: <span className="font-mono">{readable.join(", ")}</span>
              </p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
