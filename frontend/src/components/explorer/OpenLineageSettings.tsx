"use client"

/**
 * The workspace's OpenLineage export, for an admin (api-gateway openlineage_export.go):
 * where each recorded run is pushed as an OpenLineage RunEvent, and where it can be pulled.
 *
 * Its own file, with one mount point on the Lineage page, so the export can ship in another
 * edition without touching the page (docs/internal/lineage-catalog-design.md, open question 1).
 *
 * The auth header is write-only: the API says whether one is stored, never its value. A
 * save without a new value keeps the stored one; "Clear" sends "" to remove it.
 */

import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { Loader2, Share2 } from "lucide-react"

import { authFetch } from "@/lib/api/auth-fetch"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

export const OPENLINEAGE_TARGET_URL = "/api/v1/lineage/openlineage/target"
export const OPENLINEAGE_PULL_PATH = "/api/v1/lineage/openlineage"

/** openlineage_export.go openLineageTargetURLMax and openLineageAuthHeaderMax. */
const URL_MAX = 2048
const AUTH_HEADER_MAX = 4096

export interface OpenLineageTarget {
  configured: boolean
  url: string
  has_auth_header: boolean
  enabled: boolean
  /** "", delivered, failed or refused (the address is not a public one). */
  last_status: string
  last_status_code: number | null
  last_delivered_at: string | null
  updated_at: string | null
}

export function parseOpenLineageTarget(body: unknown): OpenLineageTarget {
  const b = (body && typeof body === "object" ? body : {}) as Record<string, unknown>
  const s = (v: unknown) => (typeof v === "string" ? v : "")
  return {
    configured: b.configured === true,
    url: s(b.url),
    has_auth_header: b.has_auth_header === true,
    enabled: b.enabled === true,
    last_status: s(b.last_status),
    last_status_code: typeof b.last_status_code === "number" ? b.last_status_code : null,
    last_delivered_at: s(b.last_delivered_at) || null,
    updated_at: s(b.updated_at) || null,
  }
}

/** The server's checks, said first here; the server checks again (validateOpenLineageTargetURL). */
export function validateTargetUrl(raw: string): string | null {
  const v = raw.trim()
  if (!v) return "Enter the URL to send events to."
  if (v.length > URL_MAX) return `The URL must be at most ${URL_MAX} characters.`
  let u: URL
  try {
    u = new URL(v)
  } catch {
    return "The URL must be an http or https URL with a host."
  }
  if ((u.protocol !== "http:" && u.protocol !== "https:") || !u.hostname) {
    return "The URL must be an http or https URL with a host."
  }
  if (u.username || u.password) return "Put a token in the auth header, not in the URL."
  return null
}

export interface TargetForm {
  url: string
  enabled: boolean
  /** A new value typed; "" leaves the stored one unless clearAuth. */
  authHeader: string
  clearAuth: boolean
}

/** The PUT body: auth_header absent keeps the stored one, "" clears it (PutOpenLineageTarget). */
export function targetPayload(f: TargetForm): { url: string; enabled: boolean; auth_header?: string } {
  const body: { url: string; enabled: boolean; auth_header?: string } = { url: f.url.trim(), enabled: f.enabled }
  const typed = f.authHeader.trim()
  if (typed) body.auth_header = typed
  else if (f.clearAuth) body.auth_header = ""
  return body
}

/** The last delivery, in a sentence. */
export function deliveryText(t: OpenLineageTarget): string {
  if (!t.configured) return "No target is set: events can only be pulled."
  const delivered = t.last_delivered_at ? ` Last delivered ${formatRelativeTime(t.last_delivered_at)}.` : ""
  const code = t.last_status_code ? ` (HTTP ${t.last_status_code})` : ""
  switch (t.last_status) {
    case "delivered":
      return `The last event was delivered${code}${t.last_delivered_at ? ` ${formatRelativeTime(t.last_delivered_at)}` : ""}.`
    case "failed":
      return `The last event failed${code || " (no response)"}.${delivered}`
    case "refused":
      return `The last event was not sent: the address is not a public one.${delivered}`
    default:
      return t.enabled ? "Nothing sent yet: the next run that ends is." : "Nothing sent yet. Sending is off."
  }
}

/** The Lineage page's button, for an admin only: the API refuses everyone else. */
export function OpenLineageButton() {
  const { meets, isLoading } = useWorkspaceRole()
  const [open, setOpen] = useState(false)
  if (isLoading || !meets("admin")) return null
  return (
    <>
      <Button
        variant="outline"
        size="sm"
        className="h-7 shrink-0"
        title="Send each run's lineage to a catalog that reads OpenLineage"
        onClick={() => setOpen(true)}
      >
        <Share2 className="mr-1.5 h-3.5 w-3.5" aria-hidden />
        OpenLineage
      </Button>
      {open && <OpenLineageDialog onOpenChange={setOpen} />}
    </>
  )
}

type Loaded = { target: OpenLineageTarget } | { error: string }

export function OpenLineageDialog({ onOpenChange }: { onOpenChange: (open: boolean) => void }) {
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [retryTick, setRetryTick] = useState(0)
  const [form, setForm] = useState<TargetForm>({ url: "", enabled: true, authHeader: "", clearAuth: false })
  const [busy, setBusy] = useState<"save" | "remove" | null>(null)
  const [confirmRemove, setConfirmRemove] = useState(false)
  // The disabled prop lands a render later; a fast double-click must not send twice.
  const busyRef = useRef(false)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await authFetch(OPENLINEAGE_TARGET_URL, { method: "GET", cache: "no-store" })
        const data = await res.json().catch(() => null)
        if (cancelled) return
        if (!res.ok) {
          setLoaded({ error: errorOf(data) || `Could not read the OpenLineage target (HTTP ${res.status}).` })
          return
        }
        const target = parseOpenLineageTarget(data)
        setLoaded({ target })
        setForm({ url: target.url, enabled: target.configured ? target.enabled : true, authHeader: "", clearAuth: false })
      } catch {
        if (!cancelled) setLoaded({ error: "Could not reach the server to read the OpenLineage target." })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [retryTick])

  const target = loaded && "target" in loaded ? loaded.target : null
  const loadError = loaded && "error" in loaded ? loaded.error : null

  const apply = (t: OpenLineageTarget) => {
    setLoaded({ target: t })
    setForm({ url: t.url, enabled: t.configured ? t.enabled : true, authHeader: "", clearAuth: false })
  }

  const save = async () => {
    if (busyRef.current) return
    const invalid = validateTargetUrl(form.url)
    if (invalid) {
      toast.error(invalid)
      return
    }
    if (form.authHeader.length > AUTH_HEADER_MAX) {
      toast.error(`The auth header must be at most ${AUTH_HEADER_MAX} characters.`)
      return
    }
    busyRef.current = true
    setBusy("save")
    try {
      const res = await authFetch(OPENLINEAGE_TARGET_URL, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(targetPayload(form)),
      })
      const data = await res.json().catch(() => null)
      if (!res.ok) {
        toast.error(errorOf(data) || `Could not save the OpenLineage target (HTTP ${res.status}).`)
        return
      }
      const saved = parseOpenLineageTarget(data)
      apply(saved)
      toast.success(saved.enabled ? "Saved. Each run that ends from now on is sent." : "Saved. Sending is off.")
    } catch {
      toast.error("Could not reach the server to save the OpenLineage target.")
    } finally {
      busyRef.current = false
      setBusy(null)
    }
  }

  const remove = async () => {
    if (busyRef.current) return
    if (!confirmRemove) {
      setConfirmRemove(true)
      return
    }
    busyRef.current = true
    setBusy("remove")
    try {
      const res = await authFetch(OPENLINEAGE_TARGET_URL, { method: "DELETE" })
      if (!res.ok) {
        const data = await res.json().catch(() => null)
        toast.error(errorOf(data) || `Could not remove the OpenLineage target (HTTP ${res.status}).`)
        return
      }
      apply(parseOpenLineageTarget({}))
      toast.success("Removed. Events can still be pulled.")
    } catch {
      toast.error("Could not reach the server to remove the OpenLineage target.")
    } finally {
      busyRef.current = false
      setBusy(null)
      setConfirmRemove(false)
    }
  }

  const authPlaceholder = target?.has_auth_header && !form.clearAuth ? "Stored. Type a new value to replace it" : "Optional, such as Bearer <token>"

  return (
    <Dialog open onOpenChange={onOpenChange} closeOnOutsideClick={false}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>OpenLineage</DialogTitle>
          <DialogDescription>
            Each recorded run, as an OpenLineage RunEvent: the tables it read and wrote, each named by where it lives (a
            source&apos;s host and port, a Snowflake account, otherwise the connection&apos;s id; a BigQuery table&apos;s
            project is in its name), the job&apos;s name with its kind and id, under your workspace&apos;s id, run times
            and how it ended. A model&apos;s output tables also carry their columns and the column mapping. Never the
            SQL, row counts, row values or error text. Private models are left out.
          </DialogDescription>
        </DialogHeader>

        {!loaded && (
          <p className="flex items-center gap-1.5 text-xs text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            Reading the target…
          </p>
        )}
        {loadError && (
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <p className="text-red-600 dark:text-red-400">{loadError}</p>
            <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setRetryTick((t) => t + 1)}>
              Try again
            </Button>
          </div>
        )}

        {target && (
          <div className="space-y-4">
            <section aria-label="Push" className="space-y-3">
              <h3 className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">Push</h3>
              <div className="space-y-1.5">
                <Label htmlFor="openlineage-url" className="text-xs">
                  Endpoint URL
                </Label>
                <Input
                  id="openlineage-url"
                  value={form.url}
                  placeholder="https://marquez.example.com/api/v1/lineage"
                  maxLength={URL_MAX}
                  autoComplete="off"
                  spellCheck={false}
                  onChange={(e) => setForm((f) => ({ ...f, url: e.target.value }))}
                />
                <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                  A public address: private and internal ones are refused when an event is sent.
                </p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="openlineage-auth" className="text-xs">
                  Authorization header
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="openlineage-auth"
                    type="password"
                    value={form.authHeader}
                    placeholder={authPlaceholder}
                    maxLength={AUTH_HEADER_MAX}
                    autoComplete="new-password"
                    spellCheck={false}
                    onChange={(e) => setForm((f) => ({ ...f, authHeader: e.target.value, clearAuth: false }))}
                  />
                  {target.has_auth_header && !form.clearAuth && !form.authHeader && (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-8 shrink-0 text-xs"
                      onClick={() => setForm((f) => ({ ...f, clearAuth: true }))}
                    >
                      Clear
                    </Button>
                  )}
                </div>
                <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                  {form.clearAuth
                    ? "The stored header is removed when you save."
                    : "Stored encrypted and never shown again."}
                </p>
              </div>
              <label className="flex cursor-pointer items-center gap-2 text-xs text-zinc-700 dark:text-zinc-200">
                <Switch checked={form.enabled} onCheckedChange={(enabled) => setForm((f) => ({ ...f, enabled }))} />
                Send each run as it ends
              </label>
              <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                A send is tried up to 3 times (a network error, a 429 or a 5xx is retried), waiting at most 15 seconds
                each; a redirect is not followed. Runs wait in memory: past 256 queued, or across a restart, they are not
                sent and stay in the pull.
              </p>
              <p
                className="text-xs text-zinc-600 dark:text-zinc-300"
                title={target.updated_at ? `Changed ${formatAbsoluteTime(target.updated_at, { zone: true })}` : undefined}
              >
                {deliveryText(target)}
              </p>
            </section>

            <section aria-label="Pull" className="space-y-1.5">
              <h3 className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">Pull</h3>
              <p className="text-xs text-zinc-600 dark:text-zinc-300">
                <code className="break-all rounded bg-zinc-100 px-1 py-px font-mono text-[11px] dark:bg-zinc-800">
                  GET {OPENLINEAGE_PULL_PATH}?since=&amp;until=&amp;cursor=&amp;limit=
                </code>
              </p>
              <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                An admin&apos;s request. One event per line, oldest first: 500 events a page, up to 1000 with limit=.
                since is included, until is not. A full page names the next one in the X-Next-Cursor header. Times are
                RFC 3339.
              </p>
            </section>
          </div>
        )}

        <DialogFooter className="gap-2 sm:gap-2">
          {target?.configured && (
            <Button
              variant={confirmRemove ? "destructive" : "ghost"}
              className="sm:mr-auto"
              disabled={busy !== null}
              onClick={() => void remove()}
            >
              {busy === "remove" && <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" aria-hidden />}
              {confirmRemove ? "Remove the target" : "Remove…"}
            </Button>
          )}
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Close
          </Button>
          {target && (
            <Button disabled={busy !== null} onClick={() => void save()}>
              {busy === "save" && <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" aria-hidden />}
              Save
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function errorOf(data: unknown): string {
  return data && typeof data === "object" && typeof (data as { error?: unknown }).error === "string"
    ? (data as { error: string }).error
    : ""
}
