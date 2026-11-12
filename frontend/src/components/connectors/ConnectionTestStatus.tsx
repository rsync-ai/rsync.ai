"use client"

/**
 * The stored result of a connection's last test, shared by /connections and
 * /connections/:id.
 *
 * POST /connections/:id/test persists last_tested_at / last_test_status /
 * last_test_error (api-gateway connections.go TestConnection), and every GET
 * derives is_connected from last_test_status == "success". The error is run
 * through llmscrub before it is stored, so it is safe to show as-is. A
 * "connector still being set up" answer is not a verdict and is not stored.
 */

import { RefreshCw, XCircle } from "lucide-react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { StatusPill, type PillTone } from "@/components/kit/StatusPill"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"

export type ConnectionTestState = "connected" | "expired" | "failed" | "untested"

export interface ConnectionTestFields {
  is_connected?: boolean
  // OAuth expiry from the linked oauth_tokens row (connections.go
  // applyTokenExpiry). is_expired is true only for a real past expiry.
  is_expired?: boolean
  token_expires_at?: string | null
  last_tested_at?: string | null
  last_test_status?: string | null
  last_test_error?: string | null
}

/** True when the token expired after the last test ran, or the order is unknown. */
function expiredSinceLastTest(c: ConnectionTestFields): boolean {
  if (!c.is_expired) return false
  const tested = c.last_tested_at ? Date.parse(c.last_tested_at) : NaN
  const expired = c.token_expires_at ? Date.parse(c.token_expires_at) : NaN
  if (Number.isNaN(tested) || Number.isNaN(expired)) return true
  return expired > tested
}

/**
 * Whichever fact is newer wins. A passing test proves the credentials worked
 * when it ran, so it beats a token that had already expired by then; a token
 * that expired after that test beats the stored success
 * (KI-CONNECTION-BADGE-OUTRANKS-EXPIRED-TOKEN). An expired token wins over a
 * failed test, because re-authenticating is the fix to offer first.
 */
export function connectionTestState(c: ConnectionTestFields): ConnectionTestState {
  if (expiredSinceLastTest(c)) return "expired"
  if (c.is_connected || c.last_test_status === "success") return "connected"
  if (c.is_expired) return "expired"
  if (c.last_test_status === "failed") return "failed"
  return "untested"
}

function when(iso?: string | null): { relative: string; absolute: string } | null {
  if (!iso) return null
  const absolute = formatAbsoluteTime(iso)
  if (!absolute) return null
  return { relative: formatRelativeTime(iso), absolute }
}

const NO_MESSAGE = "The connector returned no error message."

const TEST_STATE_PILL: Record<ConnectionTestState, { tone: PillTone; label: string }> = {
  connected: { tone: "ok", label: "Connected" },
  expired: { tone: "err", label: "Token expired" },
  failed: { tone: "fail", label: "Test failed" },
  untested: { tone: "skip", label: "Not tested" },
}

/** The words the badge uses for a test state, for a page that shows the state without the pill. */
export function connectionTestLabel(state: ConnectionTestState): string {
  return TEST_STATE_PILL[state].label
}

export function ConnectionTestBadge({ connection }: { connection: ConnectionTestFields }) {
  const state = connectionTestState(connection)
  const { tone, label } = TEST_STATE_PILL[state]
  // A success is only as good as its age, so the pill says it.
  const t = state === "connected" ? when(connection.last_tested_at) : null
  return (
    <StatusPill tone={tone} title={t ? `Last tested ${t.absolute}` : undefined} data-testid="connection-test-badge">
      {label}
      {t ? ` · ${t.relative}` : ""}
    </StatusPill>
  )
}

/** One line under a connection in the list: why its last test failed. */
export function LastTestErrorLine({ connection }: { connection: ConnectionTestFields }) {
  if (connectionTestState(connection) !== "failed") return null
  const t = when(connection.last_tested_at)
  const message = connection.last_test_error?.trim() || NO_MESSAGE
  return (
    <p
      className="mb-2 line-clamp-2 break-words text-xs text-red-700 dark:text-red-400"
      title={message}
      data-testid="connection-last-test-error"
    >
      <span className="font-medium">Last test failed{t ? ` ${t.relative}` : ""}:</span> {message}
    </p>
  )
}

/** The detail page's explanation of a failed last test, with a way to re-run it. */
export function LastTestFailedAlert({
  connection,
  onRetest,
  testing,
}: {
  connection: ConnectionTestFields
  onRetest: () => void
  testing: boolean
}) {
  if (connectionTestState(connection) !== "failed") return null
  const t = when(connection.last_tested_at)
  const message = connection.last_test_error?.trim() || NO_MESSAGE
  return (
    <Alert variant="destructive" data-testid="connection-last-test-failed">
      <XCircle className="h-4 w-4" />
      <AlertTitle>
        The last connection test failed
        {t && (
          <span className="font-normal" title={t.absolute}>
            {" "}
            · {t.relative}
          </span>
        )}
      </AlertTitle>
      <AlertDescription className="space-y-3">
        <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-red-50 p-2 font-mono text-xs text-red-900 dark:bg-red-950/40 dark:text-red-200">
          {message}
        </pre>
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="text-zinc-600 dark:text-zinc-400">Fix the settings with Edit Connection, then test again.</span>
          <Button variant="outline" size="sm" onClick={onRetest} disabled={testing}>
            <RefreshCw className={`h-3.5 w-3.5 mr-1.5 ${testing ? "animate-spin" : ""}`} />
            {testing ? "Testing..." : "Test again"}
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  )
}

/** "Succeeded · 5m ago" / "Failed · 5m ago" / "Never" for the detail page's facts grid. */
export function lastTestSummary(c: ConnectionTestFields): string {
  const t = when(c.last_tested_at)
  if (!c.last_test_status || !t) return "Never"
  const verdict = c.last_test_status === "success" ? "Succeeded" : c.last_test_status === "failed" ? "Failed" : c.last_test_status
  return `${verdict} · ${t.relative}`
}
