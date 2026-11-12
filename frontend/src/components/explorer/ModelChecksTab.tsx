"use client"

// The Checks tab on a model's page (docs/internal/data-checks-design.md): each data check
// with what it found on the last runs, the form to add or change one, Test now, and the
// custom SQL checks waiting for an admin. Every rule the form applies is the gateway's
// (modelChecks.ts mirrors normalizeModelCheck); the gateway still decides.

import { useEffect, useRef, useState } from "react"
import { toast } from "sonner"
import { ChevronDown, ChevronRight, FlaskConical, Loader2, Plus, SquarePen, Trash2 } from "lucide-react"

import { authFetch } from "@/lib/api/auth-fetch"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { useWarehouseTables, type WarehouseTables } from "@/components/explorer/AssetColumns"
import { findWarehouseTable } from "@/components/explorer/assetLineage"
import { CheckResultList, checkStatusBadge } from "@/components/explorer/ModelCheckResults"
import {
  ASSISTANT_DRAFT_EVENT,
  asSentence,
  CHECK_TYPES,
  cannotBlockNotice,
  changeDraftType,
  checkTypeLabel,
  describeCheckSettings,
  describeObserved,
  draftFromCheck,
  emptyDraft,
  takeAssistantDraft,
  validateDraft,
  type CheckDraft,
  type CheckProposal,
  type CheckResult,
  type CheckType,
  type ModelCheck,
  type ModelChecksResponse,
} from "@/components/explorer/modelChecks"

const SELECT_CLASS =
  "h-9 rounded-md border border-zinc-300 bg-white px-2 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:opacity-60 dark:border-zinc-700 dark:bg-zinc-900"

export interface ModelChecksTabProps {
  modelId: string
  /** The warehouse the model writes to: its schema lists the columns a check can name. */
  connectionId?: string
  targetTable?: string
  /** A schedule runs the model, so a new or changed custom SQL query waits for an admin. */
  scheduled: boolean
  /** The model's creator or a workspace admin (canEditSavedQuery). */
  canEdit: boolean
  isAdmin: boolean
  userId?: string
  reloadTick?: number
}

/** The columns the form offers, from the warehouse's schema; otherwise they are typed. */
export type ColumnChoice =
  | { status: "loading" }
  | { status: "columns"; columns: string[] }
  | { status: "typed"; note: string }

export function columnChoice(targetTable: string | undefined, tables: WarehouseTables | undefined): ColumnChoice {
  if (!targetTable) return { status: "typed", note: "This model names no table, so its columns are not listed." }
  if (!tables || tables.status === "loading") return { status: "loading" }
  if (tables.status === "error") return { status: "typed", note: tables.message }
  // A quoted name ("sales"."orders", [sales].[orders]) is looked up by its bare parts.
  const name = targetTable.replace(/["`[\]]/g, "")
  const hit = findWarehouseTable(name, tables.tables)
  if (hit.found === "many") return { status: "typed", note: `More than one table is named ${name}.` }
  if (hit.found === "none") {
    return { status: "typed", note: `${name} is not in the warehouse's schema yet; it is listed after the model's first run.` }
  }
  const columns = (hit.table.columns ?? []).map((c) => c.name)
  if (columns.length === 0) return { status: "typed", note: `The warehouse lists no columns for ${name}.` }
  return { status: "columns", columns }
}

async function errorText(res: Response, fallback: string): Promise<string> {
  const body = (await res.json().catch(() => null)) as { error?: unknown } | null
  return typeof body?.error === "string" && body.error ? body.error : `${fallback} (HTTP ${res.status}).`
}

function normalizeResponse(body: unknown): ModelChecksResponse {
  const b = (body ?? {}) as Partial<ModelChecksResponse>
  return {
    checks: Array.isArray(b.checks) ? b.checks.map((c) => ({ ...c, history: Array.isArray(c.history) ? c.history : [] })) : [],
    pending: Array.isArray(b.pending) ? b.pending : [],
    max_checks: typeof b.max_checks === "number" ? b.max_checks : 20,
    can_block: b.can_block === true,
    unsupported_reason: b.unsupported_reason || undefined,
    cannot_block_reason: b.cannot_block_reason || undefined,
    connector_type: b.connector_type || undefined,
  }
}

type TestState =
  | { status: "idle" }
  | { status: "running" }
  | { status: "done"; results: CheckResult[]; testedAt?: string }
  | { status: "error"; message: string }

interface FormState {
  key: number
  checkId: string | null
  draft: CheckDraft
  /** The Ask rsync.ai draft this form was opened from, removed once it is saved. */
  askIndex?: number
}

export function ModelChecksTab({
  modelId,
  connectionId,
  targetTable,
  scheduled,
  canEdit,
  isAdmin,
  userId,
  reloadTick = 0,
}: ModelChecksTabProps) {
  const base = `/api/v1/explorer/saved/${encodeURIComponent(modelId)}`
  const [tick, setTick] = useState(0)
  const loadKey = `${modelId}|${reloadTick}|${tick}`
  const [loaded, setLoaded] = useState<{ modelId: string; key: string; data: ModelChecksResponse } | null>(null)
  const [loadError, setLoadError] = useState<{ key: string; message: string } | null>(null)
  const [askDrafts, setAskDrafts] = useState<CheckDraft[]>([])
  const askTaken = useRef<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await authFetch(`${base}/checks`, { cache: "no-store" })
        if (!res.ok) {
          const message = await errorText(res, "Could not load the checks")
          if (!cancelled) setLoadError({ key: loadKey, message })
          return
        }
        const data = normalizeResponse(await res.json().catch(() => null))
        if (cancelled) return
        setLoaded({ modelId, key: loadKey, data })
        // A draft Ask rsync.ai handed over for this model, read once: the card stores it
        // just before it opens this tab.
        if (askTaken.current !== modelId) {
          askTaken.current = modelId
          const drafts = takeAssistantDraft(modelId)
          if (drafts) setAskDrafts(drafts)
        }
      } catch {
        if (!cancelled) setLoadError({ key: loadKey, message: "Could not reach the server to load the checks." })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [base, modelId, loadKey])
  const reload = () => setTick((t) => t + 1)

  // The tab is already open on this model when the card stores a draft: take it now.
  useEffect(() => {
    const onDraft = (e: Event) => {
      if ((e as CustomEvent<string>).detail !== modelId) return
      const drafts = takeAssistantDraft(modelId)
      if (drafts) setAskDrafts((prev) => [...prev, ...drafts])
    }
    window.addEventListener(ASSISTANT_DRAFT_EVENT, onDraft)
    return () => window.removeEventListener(ASSISTANT_DRAFT_EVENT, onDraft)
  }, [modelId])

  const warehouse = useWarehouseTables()
  const [form, setForm] = useState<FormState | null>(null)
  const [deleting, setDeleting] = useState<ModelCheck | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())
  const [test, setTest] = useState<TestState>({ status: "idle" })

  const data = loaded?.modelId === modelId ? loaded.data : null
  const error = loadError?.key === loadKey ? loadError.message : null

  if (!data) {
    return (
      <Card className="p-4" data-testid="model-checks-card">
        {error ? (
          <div className="flex flex-col items-center gap-2 py-6 text-center">
            <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
            <Button size="sm" variant="outline" onClick={reload}>
              Retry
            </Button>
          </div>
        ) : (
          <div className="flex items-center justify-center gap-2 py-6 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" />
            Loading checks…
          </div>
        )}
      </Card>
    )
  }

  const canBlock = data.can_block
  const atCap = data.checks.length >= data.max_checks
  const canAdd = canEdit && !data.unsupported_reason && !atCap
  const waitingFor = new Set(data.pending.map((p) => p.check_id).filter(Boolean) as string[])
  const choice = columnChoice(targetTable, connectionId ? warehouse.tablesOf(connectionId) : undefined)

  const openForm = (checkId: string | null, draft: CheckDraft, askIndex?: number) => {
    if (connectionId) warehouse.load(connectionId)
    // Where the engine cannot block, the gateway refuses a block, so a draft starts as a warning.
    setForm({ key: Date.now(), checkId, draft: canBlock ? draft : { ...draft, severity: "warn" }, askIndex })
  }

  const onSaved = (outcome: { pending?: string; message?: string }) => {
    if (form?.askIndex !== undefined) {
      const i = form.askIndex
      setAskDrafts((d) => d.filter((_, j) => j !== i))
    }
    setForm(null)
    if (outcome.pending) toast.info(outcome.pending)
    else toast.success(outcome.message ?? "Check saved")
    reload()
  }

  const setEnabled = async (c: ModelCheck, enabled: boolean) => {
    const v = validateDraft({ ...draftFromCheck(c), enabled }, canBlock)
    if (!v.ok) {
      toast.error(asSentence(v.error))
      return
    }
    setBusy(c.check_id)
    try {
      const res = await authFetch(`${base}/checks/${encodeURIComponent(c.check_id)}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(v.body),
      })
      if (!res.ok) toast.error(await errorText(res, "Could not change the check"))
      else toast.success(enabled ? `${c.name} is on` : `${c.name} is off`)
    } catch {
      toast.error("Could not reach the server to change the check.")
    } finally {
      setBusy(null)
      reload()
    }
  }

  const remove = async (c: ModelCheck) => {
    setBusy(c.check_id)
    try {
      const res = await authFetch(`${base}/checks/${encodeURIComponent(c.check_id)}`, { method: "DELETE" })
      if (!res.ok) toast.error(await errorText(res, "Could not remove the check"))
      else toast.success(`Removed ${c.name}`)
    } catch {
      toast.error("Could not reach the server to remove the check.")
    } finally {
      setBusy(null)
      setDeleting(null)
      reload()
    }
  }

  const runTest = async () => {
    setTest({ status: "running" })
    try {
      const res = await authFetch(`${base}/checks/test`, { method: "POST" })
      if (!res.ok) {
        setTest({ status: "error", message: await errorText(res, "Could not test the checks") })
        return
      }
      const body = (await res.json().catch(() => null)) as { results?: CheckResult[]; tested_at?: string } | null
      setTest({ status: "done", results: Array.isArray(body?.results) ? body.results : [], testedAt: body?.tested_at })
    } catch {
      setTest({ status: "error", message: "Could not reach the server to test the checks." })
    }
  }

  // A test result opens its check's card: what the check is, and its run history.
  const showCheck = (checkId: string) => {
    setExpanded((s) => new Set(s).add(checkId))
    document.getElementById(`model-check-${checkId}`)?.scrollIntoView?.({ behavior: "smooth", block: "nearest" })
  }

  const toggle = (checkId: string) =>
    setExpanded((s) => {
      const next = new Set(s)
      if (next.has(checkId)) next.delete(checkId)
      else next.add(checkId)
      return next
    })

  return (
    <div className="space-y-4">
      <Card className="p-0" data-testid="model-checks-card">
        <div className="flex flex-wrap items-start justify-between gap-2 border-b px-4 py-3 dark:border-zinc-800">
          <div className="min-w-0 space-y-0.5">
            <h2 className="text-sm font-semibold text-zinc-900 dark:text-white">
              Data checks{" "}
              <span className="font-normal text-zinc-500 dark:text-zinc-400">
                {data.checks.length} of {data.max_checks}
              </span>
            </h2>
            <p className="text-xs text-zinc-500 dark:text-zinc-400">
              {canBlock
                ? "Each run checks the table it built. A blocking check that fails leaves the live table as it was and skips the models that run after this one; a warning only alerts."
                : "Each run checks the table it built. A check that fails raises an alert."}
            </p>
          </div>
          <div className="flex flex-wrap gap-1.5">
            {data.checks.length > 0 && !data.unsupported_reason && (
              <Button size="sm" variant="outline" onClick={runTest} disabled={test.status === "running"}>
                {test.status === "running" ? (
                  <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />
                ) : (
                  <FlaskConical className="mr-1 h-3.5 w-3.5" />
                )}
                Test now
              </Button>
            )}
            {canEdit && !data.unsupported_reason && (
              <Button
                size="sm"
                onClick={() => openForm(null, emptyDraft(canBlock))}
                disabled={!canAdd}
                title={atCap ? `A model can have at most ${data.max_checks} checks` : undefined}
              >
                <Plus className="mr-1 h-3.5 w-3.5" />
                Add check
              </Button>
            )}
          </div>
        </div>

        {(data.unsupported_reason || data.cannot_block_reason || error) && (
          <div className="space-y-1 border-b px-4 py-2 text-xs dark:border-zinc-800">
            {data.unsupported_reason && (
              <p className="text-amber-700 dark:text-amber-400">{"This model can't have data checks: "}{data.unsupported_reason}</p>
            )}
            {data.cannot_block_reason && (
              <p className="text-amber-700 dark:text-amber-400">
                {cannotBlockNotice(data.connector_type)}
              </p>
            )}
            {error && <p className="text-red-600 dark:text-red-400">{error}</p>}
          </div>
        )}

        {test.status === "done" || test.status === "error" ? (
          <div className="space-y-2 border-b px-4 py-3 dark:border-zinc-800" data-testid="model-checks-test">
            <div className="flex items-center justify-between gap-2">
              <h3 className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
                Test now{test.status === "done" && test.testedAt ? ` · ${formatAbsoluteTime(test.testedAt)}` : ""}
              </h3>
              <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setTest({ status: "idle" })}>
                Close
              </Button>
            </div>
            {test.status === "error" ? (
              <p className="text-xs text-red-600 dark:text-red-400">{test.message}</p>
            ) : test.results.length === 0 ? (
              <p className="text-xs text-zinc-500 dark:text-zinc-400">There are no checks to test.</p>
            ) : (
              <>
                <CheckResultList results={test.results} onSelect={showCheck} />
                <p className="text-xs text-zinc-500 dark:text-zinc-400">
                  Checked against the live table. A test is not a run: it records nothing and alerts no one.
                </p>
              </>
            )}
          </div>
        ) : null}

        {data.checks.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-zinc-500 dark:text-zinc-400">
            {data.unsupported_reason
              ? "No checks."
              : canEdit
                ? canBlock
                  ? "No checks yet. Add one to stop a bad build from replacing the table."
                  : "No checks yet. Add one to be alerted when a build looks wrong."
                : "No checks yet."}
          </p>
        ) : (
          <ul className="divide-y dark:divide-zinc-800" aria-label="Checks">
            {data.checks.map((c) => {
              const last = c.history[0]
              const open = expanded.has(c.check_id)
              const waiting = waitingFor.has(c.check_id)
              const blocks = canBlock && c.severity === "block"
              // A test records nothing, so the run line still says what the last run found:
              // what the test found is said beside it, until the result box is closed.
              const tested = test.status === "done" ? test.results.find((r) => r.check_id === c.check_id) : undefined
              const testedFound = tested ? describeObserved(tested) : ""
              return (
                <li key={c.check_id} id={`model-check-${c.check_id}`} className="px-4 py-3" data-testid="model-check">
                  <div className="flex flex-wrap items-start justify-between gap-2">
                    <div className="min-w-0 space-y-1">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="break-words text-sm font-medium text-zinc-900 dark:text-white">{c.name}</span>
                        <Badge variant="outline" className="text-zinc-600 dark:text-zinc-300">
                          {checkTypeLabel(c.type)}
                        </Badge>
                        {/* A run treats a block its engine cannot hold as a warning (saved_query_checks.go). */}
                        <Badge
                          variant="outline"
                          className={blocks ? "border-red-500 text-red-600" : "border-amber-500 text-amber-600"}
                        >
                          {blocks ? "Blocks" : "Warns"}
                        </Badge>
                        {!c.enabled && (
                          <Badge variant="outline" className="text-zinc-500 dark:text-zinc-400">
                            Off
                          </Badge>
                        )}
                        {waiting && (
                          <Badge variant="outline" className="border-violet-500 text-violet-600">
                            Change waiting for approval
                          </Badge>
                        )}
                      </div>
                      <p className="text-xs text-zinc-600 dark:text-zinc-300">{describeCheckSettings(c)}</p>
                      <p className="flex flex-wrap items-center gap-1.5 text-xs text-zinc-500 dark:text-zinc-400">
                        {last ? (
                          <>
                            {checkStatusBadge(last.status)}
                            <span title={formatAbsoluteTime(last.finished_at)}>
                              {formatRelativeTime(last.finished_at)}
                            </span>
                            {describeObserved({ type: c.type, observed: last.observed }) && (
                              <span>· found {describeObserved({ type: c.type, observed: last.observed })}</span>
                            )}
                          </>
                        ) : (
                          <span>Not run yet</span>
                        )}
                      </p>
                      {tested && (
                        <p
                          className="flex flex-wrap items-center gap-1.5 text-xs text-zinc-500 dark:text-zinc-400"
                          data-testid="model-check-tested"
                        >
                          <span>Test now:</span>
                          {checkStatusBadge(tested.status)}
                          {testedFound && <span>found {testedFound}</span>}
                          {tested.threshold && <span>· allowed: {tested.threshold}</span>}
                        </p>
                      )}
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 px-2 text-xs"
                        aria-expanded={open}
                        onClick={() => toggle(c.check_id)}
                      >
                        {open ? <ChevronDown className="mr-1 h-3.5 w-3.5" /> : <ChevronRight className="mr-1 h-3.5 w-3.5" />}
                        History
                      </Button>
                      {canEdit && (
                        <>
                          <Button
                            size="sm"
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            disabled={busy === c.check_id || waiting}
                            onClick={() => setEnabled(c, !c.enabled)}
                          >
                            {c.enabled ? "Turn off" : "Turn on"}
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            className="h-7 px-2"
                            aria-label={`Edit ${c.name}`}
                            title={waiting ? "A change to this check is waiting for approval" : undefined}
                            disabled={busy === c.check_id || waiting}
                            onClick={() => openForm(c.check_id, draftFromCheck(c))}
                          >
                            <SquarePen className="h-3.5 w-3.5" />
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            className="h-7 px-2 text-red-600"
                            aria-label={`Remove ${c.name}`}
                            disabled={busy === c.check_id}
                            onClick={() => setDeleting(c)}
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                          </Button>
                        </>
                      )}
                    </div>
                  </div>
                  {open && (
                    <div className="mt-2 space-y-2">
                      {c.type === "custom_sql" && c.sql_text && (
                        <pre className="max-h-40 overflow-auto rounded bg-zinc-50 p-2 text-xs dark:bg-zinc-900">{c.sql_text}</pre>
                      )}
                      {c.history.length === 0 ? (
                        <p className="text-xs text-zinc-500 dark:text-zinc-400">No run has checked it yet.</p>
                      ) : (
                        <ul className="space-y-1" aria-label={`${c.name} history`}>
                          {c.history.map((h) => {
                            const observed = describeObserved({ type: c.type, observed: h.observed })
                            return (
                              <li key={h.run_id} className="flex flex-wrap items-center gap-1.5 text-xs">
                                {checkStatusBadge(h.status)}
                                <span className="text-zinc-600 dark:text-zinc-300">{formatAbsoluteTime(h.finished_at)}</span>
                                {observed && <span className="text-zinc-500 dark:text-zinc-400">· found {observed}</span>}
                                {h.threshold && <span className="text-zinc-500 dark:text-zinc-400">· allowed: {h.threshold}</span>}
                                {h.message && <span className="basis-full break-words text-zinc-500 dark:text-zinc-400">{h.message}</span>}
                              </li>
                            )
                          })}
                        </ul>
                      )}
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </Card>

      {askDrafts.length > 0 && (
        <Card className="space-y-2 p-4" data-testid="model-checks-ask-drafts">
          <div className="flex items-center justify-between gap-2">
            <h3 className="text-sm font-semibold text-zinc-900 dark:text-white">Drafted by Ask rsync.ai</h3>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" onClick={() => setAskDrafts([])}>
              Discard all
            </Button>
          </div>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Nothing is saved until you review a draft and add it.
            {!canEdit && " Only the model's creator or a workspace admin can add checks."}
          </p>
          <ul className="space-y-1.5">
            {askDrafts.map((d, i) => (
              <li key={`${d.name}-${i}`} className="flex flex-wrap items-center justify-between gap-2 rounded border px-2 py-1.5 dark:border-zinc-800">
                <span className="min-w-0 text-xs">
                  <span className="font-medium">{d.name || "Unnamed check"}</span>
                  <span className="text-zinc-500 dark:text-zinc-400">
                    {" "}
                    · {checkTypeLabel(d.type)}
                    {d.columns.length > 0 && ` · ${d.columns.join(", ")}`} · {canBlock && d.severity === "block" ? "blocks" : "warns"}
                  </span>
                </span>
                <span className="flex gap-1">
                  <Button
                    size="sm"
                    variant="outline"
                    className="h-7 px-2 text-xs"
                    disabled={!canAdd}
                    onClick={() => openForm(null, d, i)}
                  >
                    Review and add
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="h-7 px-2 text-xs"
                    onClick={() => setAskDrafts((all) => all.filter((_, j) => j !== i))}
                  >
                    Discard
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        </Card>
      )}

      {data.pending.length > 0 && (
        <Card className="space-y-3 p-4" data-testid="model-checks-pending">
          <h3 className="text-sm font-semibold text-zinc-900 dark:text-white">Waiting for approval</h3>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            A schedule runs this model as its owner, so a new or changed custom SQL query takes effect only once a
            workspace admin approves it.
          </p>
          {data.pending.map((p) => (
            <PendingCheck key={p.id} base={base} proposal={p} isAdmin={isAdmin} userId={userId} onReviewed={reload} />
          ))}
        </Card>
      )}

      {form && (
        <CheckFormDialog
          key={form.key}
          base={base}
          checkId={form.checkId}
          initial={form.draft}
          canBlock={canBlock}
          connectorType={data.connector_type}
          scheduled={scheduled}
          choice={choice}
          onClose={() => setForm(null)}
          onSaved={onSaved}
        />
      )}

      <AlertDialog open={!!deleting} onOpenChange={(o) => !o && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove {deleting?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Runs stop checking it. What it found on past runs stays in their results.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => deleting && remove(deleting)}>Remove</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function PendingCheck({
  base,
  proposal: p,
  isAdmin,
  userId,
  onReviewed,
}: {
  base: string
  proposal: CheckProposal
  isAdmin: boolean
  userId?: string
  onReviewed: () => void
}) {
  const [note, setNote] = useState("")
  const [busy, setBusy] = useState(false)
  const review = async (verdict: "approve" | "reject") => {
    setBusy(true)
    try {
      const res = await authFetch(`${base}/checks/pending/${encodeURIComponent(p.id)}/${verdict}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(note.trim() ? { note: note.trim() } : {}),
      })
      if (!res.ok) toast.error(await errorText(res, "Could not review this change"))
      else toast.success(verdict === "approve" ? `Approved ${p.check.name}` : `Rejected ${p.check.name}`)
    } catch {
      toast.error("Could not reach the server to review this change.")
    } finally {
      setBusy(false)
      onReviewed()
    }
  }
  return (
    <div className="space-y-1.5 rounded border p-3 text-xs dark:border-zinc-800" data-testid="model-check-proposal">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-sm font-medium text-zinc-900 dark:text-white">{p.check.name}</span>
        <span className="text-zinc-500 dark:text-zinc-400">
          {p.check_id ? "changes this check's query" : "a new check"} · proposed{" "}
          {p.proposed_by && p.proposed_by === userId ? "by you " : ""}
          <span title={formatAbsoluteTime(p.proposed_at)}>{formatRelativeTime(p.proposed_at)}</span>
        </span>
      </div>
      <pre className="max-h-40 overflow-auto rounded bg-zinc-50 p-2 dark:bg-zinc-900">{p.sql_text}</pre>
      {p.base_sql_text && p.base_sql_text !== p.sql_text && (
        <details>
          <summary className="cursor-pointer text-zinc-500 dark:text-zinc-400">The query it replaces</summary>
          <pre className="mt-1 max-h-40 overflow-auto rounded bg-zinc-50 p-2 dark:bg-zinc-900">{p.base_sql_text}</pre>
        </details>
      )}
      {p.note && <p className="text-zinc-600 dark:text-zinc-300">Note: {p.note}</p>}
      {isAdmin ? (
        <div className="flex flex-wrap items-center gap-1.5 pt-1">
          <Input
            aria-label="Review note"
            placeholder="Note (optional)"
            className="h-8 max-w-xs text-xs"
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
          <Button size="sm" className="h-8" disabled={busy} onClick={() => review("approve")}>
            Approve
          </Button>
          <Button size="sm" variant="outline" className="h-8" disabled={busy} onClick={() => review("reject")}>
            Reject
          </Button>
        </div>
      ) : (
        <p className="text-zinc-500 dark:text-zinc-400">A workspace admin approves or rejects it.</p>
      )}
    </div>
  )
}

function CheckFormDialog({
  base,
  checkId,
  initial,
  canBlock,
  connectorType,
  scheduled,
  choice,
  onClose,
  onSaved,
}: {
  base: string
  checkId: string | null
  initial: CheckDraft
  canBlock: boolean
  /** With !canBlock: the engine the notice names. */
  connectorType?: string
  scheduled: boolean
  choice: ColumnChoice
  onClose: () => void
  onSaved: (outcome: { pending?: string; message?: string }) => void
}) {
  const [draft, setDraft] = useState<CheckDraft>(initial)
  const [columnsText, setColumnsText] = useState(initial.columns.join(", "))
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [tested, setTested] = useState<CheckResult[] | "running" | null>(null)
  const update = (patch: Partial<CheckDraft>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setError(null)
    setTested(null)
  }
  const typeInfo = CHECK_TYPES.find((t) => t.value === draft.type)
  const needsColumns = draft.type === "not_null" || draft.type === "unique"
  // A column the check already names stays offered, though the schema no longer lists it.
  const listed =
    choice.status === "columns"
      ? [...choice.columns, ...draft.columns.filter((c) => !choice.columns.includes(c))]
      : []

  // Test runs this check, as it would save, on the live table: nothing is saved, recorded
  // or sent, so a check can be tried before it is added.
  const testDraft = async () => {
    // A test saves nothing, so a draft with no name yet runs under its type's name.
    const v = validateDraft({ ...draft, name: draft.name.trim() || checkTypeLabel(draft.type) }, canBlock)
    if (!v.ok) {
      setError(v.error)
      return
    }
    setError(null)
    setTested("running")
    try {
      const res = await authFetch(`${base}/checks/test`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ check: v.body }),
      })
      if (!res.ok) {
        setTested(null)
        setError(await errorText(res, "Could not test the check"))
        return
      }
      const body = (await res.json().catch(() => null)) as { results?: CheckResult[] } | null
      setTested(Array.isArray(body?.results) ? body.results : [])
    } catch {
      setTested(null)
      setError("Could not reach the server to test the check.")
    }
  }

  const save = async () => {
    const v = validateDraft(draft, canBlock)
    if (!v.ok) {
      setError(v.error)
      return
    }
    setSaving(true)
    try {
      const res = await authFetch(checkId ? `${base}/checks/${encodeURIComponent(checkId)}` : `${base}/checks`, {
        method: checkId ? "PUT" : "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(v.body),
      })
      if (!res.ok) {
        setError(await errorText(res, "Could not save the check"))
        return
      }
      const body = (await res.json().catch(() => null)) as { pending_approval?: { reason?: string } } | null
      if (body?.pending_approval) {
        onSaved({ pending: body.pending_approval.reason || "The check is waiting for a workspace admin's approval." })
      } else {
        onSaved({ message: checkId ? `Saved ${draft.name.trim()}` : `Added ${draft.name.trim()}` })
      }
    } catch {
      setError("Could not reach the server to save the check.")
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} closeOnOutsideClick={false}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>{checkId ? "Edit check" : "Add check"}</DialogTitle>
          <DialogDescription>Each run of the model checks the table it built.</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            void save()
          }}
        >
          <label className="block space-y-1 text-sm">
            <span className="font-medium">Name</span>
            <Input value={draft.name} maxLength={100} onChange={(e) => update({ name: e.target.value })} />
          </label>

          <label className="block space-y-1 text-sm">
            <span className="font-medium">Type</span>
            <select
              aria-label="Check type"
              className={`${SELECT_CLASS} w-full`}
              value={draft.type}
              onChange={(e) => {
                const next = changeDraftType(draft, e.target.value as CheckType, canBlock)
                setDraft(next)
                setColumnsText(next.columns.join(", "))
                setError(null)
                setTested(null)
              }}
            >
              {CHECK_TYPES.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            {typeInfo && <span className="block text-xs text-zinc-500 dark:text-zinc-400">{typeInfo.hint}</span>}
          </label>

          {needsColumns && (
            <div className="space-y-1 text-sm">
              <span className="font-medium">{draft.type === "unique" ? "Columns" : "Column"}</span>
              {choice.status === "loading" ? (
                <p className="inline-flex items-center gap-1 text-xs text-zinc-500 dark:text-zinc-400">
                  <Loader2 className="h-3 w-3 animate-spin" /> {"Reading the table's columns…"}
                </p>
              ) : choice.status === "columns" && draft.type === "not_null" ? (
                <select
                  aria-label="Column"
                  className={`${SELECT_CLASS} w-full`}
                  value={draft.columns[0] ?? ""}
                  onChange={(e) => update({ columns: e.target.value ? [e.target.value] : [] })}
                >
                  <option value="">Choose a column</option>
                  {listed.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
              ) : choice.status === "columns" ? (
                <fieldset className="max-h-40 space-y-0.5 overflow-auto rounded border p-2 dark:border-zinc-800" aria-label="Columns">
                  {listed.map((c) => (
                    <label key={c} className="flex items-center gap-2 text-xs">
                      <input
                        type="checkbox"
                        checked={draft.columns.includes(c)}
                        onChange={(e) =>
                          update({
                            columns: e.target.checked ? [...draft.columns, c] : draft.columns.filter((x) => x !== c),
                          })
                        }
                      />
                      {c}
                    </label>
                  ))}
                </fieldset>
              ) : (
                <>
                  <Input
                    aria-label="Columns"
                    placeholder={draft.type === "unique" ? "order_id, line_no" : "order_id"}
                    value={columnsText}
                    onChange={(e) => {
                      setColumnsText(e.target.value)
                      update({
                        columns: e.target.value
                          .split(",")
                          .map((c) => c.trim())
                          .filter(Boolean),
                      })
                    }}
                  />
                  <span className="block text-xs text-zinc-500 dark:text-zinc-400">
                    {choice.note} Type the column {draft.type === "unique" ? "names, separated by commas" : "name"}.
                  </span>
                </>
              )}
            </div>
          )}

          {draft.type === "row_count" && (
            <div className="flex flex-wrap gap-3 text-sm">
              <label className="space-y-1">
                <span className="block font-medium">At least</span>
                <Input className="w-32" inputMode="numeric" value={draft.min} onChange={(e) => update({ min: e.target.value })} />
              </label>
              <label className="space-y-1">
                <span className="block font-medium">At most (optional)</span>
                <Input className="w-32" inputMode="numeric" value={draft.max} onChange={(e) => update({ max: e.target.value })} />
              </label>
            </div>
          )}

          {draft.type === "row_count_change" && (
            <div className="flex flex-wrap gap-3 text-sm">
              <label className="space-y-1">
                <span className="block font-medium">Largest drop, %</span>
                <Input
                  className="w-32"
                  inputMode="decimal"
                  value={draft.max_drop_pct}
                  onChange={(e) => update({ max_drop_pct: e.target.value })}
                />
              </label>
              <label className="space-y-1">
                <span className="block font-medium">Largest growth, % (optional)</span>
                <Input
                  className="w-32"
                  inputMode="decimal"
                  value={draft.max_growth_pct}
                  onChange={(e) => update({ max_growth_pct: e.target.value })}
                />
              </label>
            </div>
          )}

          {draft.type === "custom_sql" && (
            <label className="block space-y-1 text-sm">
              <span className="font-medium">Query</span>
              <Textarea
                aria-label="Check query"
                className="min-h-[120px] font-mono text-xs"
                value={draft.sql_text}
                onChange={(e) => update({ sql_text: e.target.value })}
              />
              <span className="block text-xs text-zinc-500 dark:text-zinc-400">
                {"One SELECT that returns the rows breaking the rule. Write {{this}} for the model's table."}
                {scheduled && " This model is on a schedule, so a new or changed query waits for a workspace admin's approval."}
              </span>
            </label>
          )}

          {draft.type === "custom_sql" && scheduled && (
            <label className="block space-y-1 text-sm">
              <span className="font-medium">Note for the approver (optional)</span>
              <Input value={draft.note} onChange={(e) => update({ note: e.target.value })} />
            </label>
          )}

          <label className="block space-y-1 text-sm">
            <span className="font-medium">When it fails</span>
            <select
              aria-label="When it fails"
              className={`${SELECT_CLASS} w-full`}
              value={canBlock ? draft.severity : "warn"}
              disabled={!canBlock}
              onChange={(e) => update({ severity: e.target.value === "block" ? "block" : "warn" })}
            >
              <option value="block">Block: keep the live table and skip what runs after</option>
              <option value="warn">Warn: publish the table and alert</option>
            </select>
            {!canBlock && (
              <span className="block text-xs text-zinc-500 dark:text-zinc-400">
                {cannotBlockNotice(connectorType)}
              </span>
            )}
          </label>

          {Array.isArray(tested) && (
            <section aria-label="Test result" className="space-y-1">
              {tested.length > 0 ? (
                <CheckResultList results={tested} />
              ) : (
                <p className="text-xs text-zinc-500 dark:text-zinc-400">The test returned no result.</p>
              )}
              <p className="text-xs text-zinc-500 dark:text-zinc-400">
                Checked against the live table. Nothing was saved, and a test alerts no one.
              </p>
            </section>
          )}

          {error && (
            <p role="alert" className="text-sm text-red-600 dark:text-red-400">
              {asSentence(error)}
            </p>
          )}

          <DialogFooter className="gap-2">
            <Button
              type="button"
              variant="outline"
              className="sm:mr-auto"
              onClick={() => void testDraft()}
              disabled={saving || tested === "running"}
              title="Run this check on the live table now, without saving it"
            >
              {tested === "running" ? (
                <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />
              ) : (
                <FlaskConical className="mr-1 h-3.5 w-3.5" />
              )}
              Test
            </Button>
            <Button type="button" variant="outline" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving}>
              {saving && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
              {checkId ? "Save" : "Add check"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
