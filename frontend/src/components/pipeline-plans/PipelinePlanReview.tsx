"use client"

import { Fragment, useCallback, useEffect, useState } from "react"
import Link from "next/link"
import { ArrowRight, Loader2 } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"
import {
  applyPipelinePlan,
  getPipelinePlan,
  PipelinePlanError,
  tablesToApply,
  type AppliedPlan,
  type PipelinePlan,
} from "@/lib/api/pipeline-plans"

/** How many tables the card lists before "and N more". The page lists all. */
const CARD_TABLES = 8

/**
 * A pipeline Ask rsync.ai or an agent proposed (plan_pipeline), for the person to
 * check and create. Nothing exists until they click Create: that POSTs the plan
 * with their session, and the server checks it again (handlers/pipeline_plans.go).
 * A name the source has in more than one schema is a choice they make here; the
 * plan never picks one. Shown as a card under an Ask rsync.ai answer (compact) and
 * as the /pipeline-plans/[id] page an agent's link opens.
 */
export function PipelinePlanReview({ planId, compact = false }: { planId: string; compact?: boolean }) {
  const [plan, setPlan] = useState<PipelinePlan | null>(null)
  const [loadError, setLoadError] = useState<PipelinePlanError | Error | null>(null)
  const [picks, setPicks] = useState<Record<string, string>>({})
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<AppliedPlan | null>(null)
  const [applyError, setApplyError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoadError(null)
    try {
      setPlan(await getPipelinePlan(planId))
    } catch (err) {
      setLoadError(err instanceof Error ? err : new Error("Could not load the plan"))
    }
  }, [planId])

  useEffect(() => {
    void load()
  }, [load])

  const create = async () => {
    if (!plan || creating) return
    setCreating(true)
    setApplyError(null)
    try {
      setCreated(await applyPipelinePlan(plan.id, tablesToApply(plan, picks)))
    } catch (err) {
      setApplyError(err instanceof Error ? err.message : "Could not create the pipeline")
      // Applied or expired elsewhere, or changed: show what it is now.
      if (err instanceof PipelinePlanError && ["already_applied", "expired", "plan_changed"].includes(err.code)) {
        await load()
      }
    } finally {
      setCreating(false)
    }
  }

  if (loadError) {
    const notFound = loadError instanceof PipelinePlanError && loadError.status === 404
    return (
      <div
        role="alert"
        className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
      >
        <span>{loadError.message}</span>
        {!notFound && (
          <button
            type="button"
            onClick={() => void load()}
            className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
          >
            Retry
          </button>
        )}
      </div>
    )
  }
  if (!plan) {
    return (
      <div role="status" className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading the proposed pipeline…
      </div>
    )
  }

  const choices = plan.table_choices ?? []
  const open = plan.status === "proposed" && !created
  const pickedAll = choices.every((c) => !!picks[c.name])
  const pipelineId = created?.pipeline_id ?? plan.applied_pipeline_id
  const shownTables = compact ? plan.tables.slice(0, CARD_TABLES) : plan.tables
  const moreTables = plan.tables.length - shownTables.length

  return (
    <section
      aria-label="Proposed pipeline"
      className="space-y-3 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 className="font-semibold text-zinc-900 dark:text-white">Proposed pipeline</h3>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Proposed by {plan.proposed_by === "agent" ? "an AI agent" : "Ask rsync.ai"}. Nothing is created until you
            click Create.
          </p>
        </div>
        <StatusBadge status={created ? "applied" : plan.status} />
      </header>

      <div className="flex flex-wrap items-center gap-2 text-zinc-900 dark:text-zinc-100">
        <ConnectionName name={plan.source.name} type={plan.source.connector_type} />
        <ArrowRight aria-label="to" className="h-4 w-4 shrink-0 text-zinc-400" />
        <ConnectionName name={plan.destination.name} type={plan.destination.connector_type} />
      </div>

      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-xs">
        <dt className="text-zinc-500 dark:text-zinc-400">Mode</dt>
        <dd className="text-zinc-800 dark:text-zinc-200">{describeMode(plan)}</dd>
        <dt className="text-zinc-500 dark:text-zinc-400">Runs</dt>
        <dd className="text-zinc-800 dark:text-zinc-200">
          {plan.schedule?.description ?? (plan.sync_mode === "cdc" ? "Continuously" : "Once, when you create it")}
        </dd>
        {plan.destination_namespace && (
          <>
            <dt className="text-zinc-500 dark:text-zinc-400">Lands in</dt>
            <dd className="break-all font-mono text-zinc-800 dark:text-zinc-200">{plan.destination_namespace}</dd>
          </>
        )}
        <dt className="text-zinc-500 dark:text-zinc-400">Tables</dt>
        <dd className="text-zinc-800 dark:text-zinc-200">
          {plan.tables.length === 0 && choices.length === 0 ? (
            "None named: you pick them when the pipeline first runs."
          ) : (
            <>
              {shownTables.length > 0 && (
                <ul aria-label="Tables" className="flex flex-wrap gap-1">
                  {shownTables.map((t) => (
                    <li key={t} className="rounded bg-zinc-100 px-1.5 py-0.5 font-mono dark:bg-zinc-800">
                      {t}
                    </li>
                  ))}
                </ul>
              )}
              {moreTables > 0 && (
                <Link href={`/pipeline-plans/${plan.id}`} className="mt-1 inline-block text-violet-700 hover:underline dark:text-violet-300">
                  and {moreTables} more
                </Link>
              )}
              {!plan.tables_checked && (
                <p className="mt-1 text-zinc-500 dark:text-zinc-400">
                  Not checked against the source: its table list was not available.
                </p>
              )}
            </>
          )}
        </dd>
      </dl>

      {open &&
        choices.map((choice) => (
          <fieldset key={choice.name} className="space-y-1.5 rounded-lg border border-amber-200 bg-amber-50 p-3 dark:border-amber-900 dark:bg-amber-950/40">
            <legend className="px-1 text-xs font-medium text-amber-900 dark:text-amber-200">
              <span className="font-mono">{choice.name}</span> is in more than one schema. Which one?
            </legend>
            <RadioGroup
              aria-label={`Schema for ${choice.name}`}
              value={picks[choice.name] ?? ""}
              onValueChange={(v) => setPicks((p) => ({ ...p, [choice.name]: v }))}
            >
              {choice.options.map((option) => {
                const id = `plan-${plan.id}-${option}`
                return (
                  <div key={option} className="flex items-center gap-2">
                    <RadioGroupItem value={option} id={id} />
                    <label htmlFor={id} className="font-mono text-xs text-zinc-800 dark:text-zinc-200">
                      {option}
                    </label>
                  </div>
                )
              })}
            </RadioGroup>
          </fieldset>
        ))}

      {plan.notes && plan.notes.length > 0 && (
        <ul aria-label="Notes" className="list-disc space-y-1 pl-4 text-xs text-zinc-600 dark:text-zinc-300">
          {plan.notes.map((n) => (
            <li key={n}>
              <NoteText text={n} />
            </li>
          ))}
        </ul>
      )}

      {applyError && (
        <p role="alert" className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200">
          {applyError}
        </p>
      )}

      <footer className="flex flex-wrap items-center gap-3">
        {open && (
          <>
            <Button size="sm" onClick={() => void create()} loading={creating} disabled={!pickedAll}>
              Create pipeline
            </Button>
            <span className="text-xs text-zinc-500 dark:text-zinc-400">
              {!pickedAll
                ? "Pick a schema for each table above first."
                : plan.schedule
                  ? "Creating it sets up its schedule."
                  : "Creating it starts its first run."}{" "}
              Open until <LocalDateTime value={plan.expires_at} />.
            </span>
          </>
        )}
        {pipelineId && (
          <>
            <span className="text-xs text-emerald-700 dark:text-emerald-300">
              {created
                ? created.status === "scheduled"
                  ? `Created. It runs ${created.schedule ?? "on its schedule"}.`
                  : "Created, and its first run started."
                : "A pipeline was created from this plan."}
            </span>
            <Button asChild size="sm" variant="outline">
              <Link href={`/pipelines/${pipelineId}`}>Open pipeline</Link>
            </Button>
          </>
        )}
        {!open && !pipelineId && plan.status === "expired" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            This plan expired 24 hours after it was proposed. Ask again for a new one.
          </span>
        )}
        {compact && open && (
          <Link href={`/pipeline-plans/${plan.id}`} className="text-xs text-violet-700 hover:underline dark:text-violet-300">
            Open as a page
          </Link>
        )}
      </footer>
      {created?.note && (
        <p className="text-xs text-zinc-600 dark:text-zinc-300">
          <NoteText text={created.note} />
        </p>
      )}
      {created?.warning && (
        <p role="alert" className="text-xs text-amber-700 dark:text-amber-300">
          {created.warning}
        </p>
      )}
    </section>
  )
}

/**
 * A server note as text, with the names it quotes in backticks (backtickList,
 * handlers/chat_nl_pipeline.go, shared with the chat replies) shown as code.
 * A note with an unpaired backtick is shown as it is.
 */
function NoteText({ text }: { text: string }) {
  const parts = text.split("`")
  if (parts.length % 2 === 0) return <>{text}</>
  return (
    <>
      {parts.map((part, i) =>
        i % 2 === 1 ? (
          <code key={i} className="rounded bg-zinc-100 px-1 font-mono dark:bg-zinc-800">
            {part}
          </code>
        ) : (
          <Fragment key={i}>{part}</Fragment>
        )
      )}
    </>
  )
}

function StatusBadge({ status }: { status: PipelinePlan["status"] }) {
  if (status === "applied") return <Badge variant="success">Created</Badge>
  if (status === "expired") return <Badge variant="secondary">Expired</Badge>
  return <Badge variant="info">Waiting for you</Badge>
}

function ConnectionName({ name, type }: { name: string; type: string }) {
  return (
    <span className="min-w-0">
      <span className="font-medium">{name}</span>{" "}
      <span className="text-xs text-zinc-500 dark:text-zinc-400">({type})</span>
    </span>
  )
}

function describeMode(plan: PipelinePlan): string {
  if (plan.sync_mode !== "cdc") return "Batch: copies the tables on each run"
  return plan.cdc_mode === "streaming_only"
    ? "CDC: streams each change from now on"
    : "CDC: copies the tables once, then streams each change"
}
