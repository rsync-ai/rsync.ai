"use client"

import { useEffect, useRef, useState } from "react"
import { Check, Copy, Sparkles } from "lucide-react"

import { AssistantMarkdown } from "@/components/chat/AssistantMarkdown"
import { DashboardChangeReview } from "@/components/dashboards/DashboardChangeReview"
import { DashboardProposalReview } from "@/components/dashboards/DashboardProposalReview"
import { ModelChecksDraftReview } from "@/components/explorer/ModelChecksDraftReview"
import { draftsFromAssistant, type AssistantChecksDraft } from "@/components/explorer/modelChecks"
import { PipelinePlanReview } from "@/components/pipeline-plans/PipelinePlanReview"
import { SqlDraftCard } from "@/components/assistant/SqlDraftCard"
import { WorkflowChangeReview } from "@/components/workflows/WorkflowChangeReview"
import { WorkflowReview } from "@/components/workflows/WorkflowReview"
import { workflowIds, type AssistantAnswer, type LookedAt, type SqlDraft } from "@/lib/api/assistant"
import { NO_MODEL } from "@/lib/assistant/ask"
import type { AssistantChat, AssistantPageRef } from "@/lib/store/useAssistantStore"

/** What "Looked at" calls each of the agent gateway's tools (api-gateway agentgw/tools.go). */
/** What each agent-gateway tool read, in words; a test checks every tool has one. */
export const TOOL_LABELS: Record<string, string> = {
  list_pipelines: "your pipelines",
  get_pipeline: "the pipeline's settings",
  get_pipeline_health: "the pipeline's health",
  get_run_status: "run status",
  diagnose_run: "the run's errors",
  list_connections: "your connections",
  describe_schema: "table schemas",
  list_runs: "run history",
  list_connectors: "the connector catalog",
  generate_connector: "connector generation",
  get_connector_generation: "the generation run",
  list_models: "your models",
  get_model: "the model's settings",
  list_model_runs: "the model's runs",
  create_model: "model creation",
  create_silver_model: "silver model creation",
  schedule_model: "the model's schedule",
  run_model: "a model run",
  pause_model_schedule: "pausing the schedule",
  resume_model_schedule: "resuming the schedule",
  list_model_checks: "the model's checks",
  plan_model_checks: "a draft of data checks",
  plan_pipeline: "a pipeline proposal",
  plan_workflow: "a workflow draft",
  list_workflows: "your workflows",
  plan_workflow_change: "a workflow change",
  list_chart_sources: "your chart sources",
  describe_chart_source: "a chart source's columns",
  plan_dashboard: "a dashboard draft",
  list_dashboards: "your dashboards",
  get_dashboard: "the dashboard's tiles",
  plan_dashboard_change: "a dashboard change",
  get_usage: "your plan and its usage",
  read_guide: "rsync.ai's guide",
  // The assistant's own tools: sample_rows only while sample rows are on.
  sample_rows: "sample rows",
  generate_sql: "a SQL draft",
}

/**
 * What "Looked at" says for one call. A sample_rows call names its table and
 * what it read: "sample rows of public.orders (3 rows; 2 columns withheld)".
 */
export function lookedAtLabel(l: LookedAt): string {
  const label = TOOL_LABELS[l.tool] ?? l.tool
  if (l.tool === "generate_sql") return l.ok && l.sql ? label : `${label} (not written)`
  if (l.tool !== "sample_rows") return label
  const table = typeof l.arguments?.table === "string" ? ` of ${l.arguments.table}` : ""
  if (!l.ok || !l.sample) return `${label}${table} (not read)`
  if (l.sample.not_sent) return `${label}${table} (not sent to the model)`
  const rows = `${l.sample.rows} ${l.sample.rows === 1 ? "row" : "rows"}`
  const withheld = l.sample.withheld_columns
    ? `; ${l.sample.withheld_columns} ${l.sample.withheld_columns === 1 ? "column" : "columns"} withheld`
    : ""
  return `${label}${table} (${rows}${withheld})`
}

/**
 * What a new chat says the assistant does. It reads, and its write tools
 * (plan_pipeline for members; generate_connector, the model tools,
 * plan_workflow, plan_workflow_change, plan_dashboard and plan_dashboard_change, owners and admins only) mean it must not claim to change
 * nothing; a test checks this against the gateway's tool reference.
 */
export const WELCOME =
  "Ask about your pipelines, connections, runs and models: it looks them up and leaves them as they are. It can propose a pipeline between two of your connections, which you check and create yourself. Workspace owners and admins can also ask it to generate a connector for an API rsync.ai does not have, to build, schedule and run silver and gold models, or to draft a workflow (a report or data alert to Slack or email), which you test and turn on yourself, to propose a change to a workflow's conditions, which you apply yourself, to draft a dashboard of charts on your models and saved queries, which you preview and save yourself, and to propose a change to one tile of a dashboard, which you apply yourself. It can draft data checks for a model, which you save on the model's Checks tab, and write SQL for a question about your data, which you open in the Explorer and run yourself."

/** Questions the tools can answer, the page's own first. */
export function starters(page: AssistantPageRef | null): string[] {
  const own =
    page?.kind === "pipeline"
      ? ["Why did this pipeline's last run fail?", "Is this pipeline healthy?"]
      : page?.kind === "connection"
        ? ["What tables does this connection have?"]
        : page?.kind === "run"
          ? ["Why did this run fail?"]
          : page?.kind === "dashboard"
            ? ["What charts are on this dashboard?", "Make the first chart a line chart"]
            : []
  return [...own, "Which pipelines are failing right now?", "Which pipelines are waiting for me?", "What connections do I have?"]
}

export function AssistantLogo({ size = "sm" }: { size?: "sm" | "lg" }) {
  return (
    <div
      aria-hidden
      className={
        size === "lg"
          ? "flex h-12 w-12 shrink-0 items-center justify-center rounded-2xl bg-gradient-to-br from-violet-500 to-indigo-600 shadow-sm"
          : "flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-gradient-to-br from-violet-500 to-indigo-600"
      }
    >
      <Sparkles className={size === "lg" ? "h-6 w-6 text-white" : "h-4 w-4 text-white"} />
    </div>
  )
}

/** A new chat: what the assistant does, and questions to start from. */
export function AssistantWelcome({
  page,
  noModels,
  onAsk,
}: {
  page: AssistantPageRef | null
  noModels: boolean
  onAsk: (question: string) => void
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center overflow-y-auto px-6 py-8 text-center">
      <AssistantLogo size="lg" />
      <h2 className="mt-3 text-lg font-semibold text-zinc-900 dark:text-white">Ask rsync.ai</h2>
      <p className="mt-1 max-w-sm text-sm text-zinc-500 dark:text-zinc-400">
        {WELCOME}
      </p>
      {noModels ? (
        <p className="mt-6 max-w-sm rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200">
          {NO_MODEL}
        </p>
      ) : (
        <ul className="mt-6 flex w-full max-w-sm flex-col gap-2" aria-label="Suggested questions">
          {starters(page).map((q) => (
            <li key={q}>
              <button
                type="button"
                onClick={() => onAsk(q)}
                className="w-full rounded-xl border border-zinc-200 bg-white px-4 py-2.5 text-left text-sm text-zinc-800 transition-colors hover:border-violet-300 hover:bg-violet-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-200 dark:hover:border-violet-800 dark:hover:bg-violet-950/40"
              >
                {q}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** The chat's questions and answers, newest at the bottom. */
export function AssistantThread({
  chat,
  wide,
  onRetry,
}: {
  chat: AssistantChat
  /** The panel fills the page: keep lines to a readable width. */
  wide: boolean
  onRetry: (question: string) => void
}) {
  const endRef = useRef<HTMLDivElement>(null)
  const { items, pendingSince } = chat

  useEffect(() => {
    endRef.current?.scrollIntoView?.({ block: "end" })
  }, [items.length, pendingSince])

  return (
    <div className="flex-1 overflow-y-auto">
      <div
        className={`mx-auto w-full space-y-5 px-4 py-5 text-sm ${wide ? "max-w-3xl" : ""}`}
        role="log"
        aria-label="Chat"
        aria-live="polite"
      >
        {/* Items are only ever appended, and only the last is ever removed (Try again), so an index is a stable key. */}
        {items.map((item, i) => {
          switch (item.kind) {
            case "question":
              return (
                <div key={i} className="flex justify-end">
                  <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-zinc-100 px-3.5 py-2 text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100">
                    {item.text}
                  </div>
                </div>
              )
            case "answer":
              return <AnswerView key={i} answer={item.answer} />
            case "error":
              return (
                <div
                  key={i}
                  role="alert"
                  className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
                >
                  <span>{item.text}</span>
                  {i === items.length - 1 && !pendingSince && (
                    <button
                      type="button"
                      onClick={() => onRetry(item.question)}
                      className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
                    >
                      Try again
                    </button>
                  )}
                </div>
              )
            case "stopped":
              return (
                <p key={i} className="text-xs italic text-zinc-500 dark:text-zinc-400">
                  Stopped before it answered.
                </p>
              )
            case "elsewhere":
              return (
                <p key={i} className="text-xs italic text-zinc-500 dark:text-zinc-400">
                  Asked before this page opened. If that page is still open, the answer shows there.
                </p>
              )
            case "forgotten":
              return (
                <div key={i} className="flex items-center gap-2 text-[11px] text-zinc-500 dark:text-zinc-400">
                  <span className="h-px flex-1 bg-zinc-200 dark:bg-zinc-800" />
                  <span className="text-center">
                    Over 30 minutes since the last answer: the assistant no longer remembers the questions above.
                  </span>
                  <span className="h-px flex-1 bg-zinc-200 dark:bg-zinc-800" />
                </div>
              )
          }
        })}
        {pendingSince !== undefined && <Thinking since={pendingSince} />}
        <div ref={endRef} />
      </div>
    </div>
  )
}

function Thinking({ since }: { since: number }) {
  const [now, setNow] = useState(since)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const seconds = Math.max(0, Math.floor((now - since) / 1000))
  return (
    <div className="flex items-center gap-2.5" role="status">
      <AssistantLogo />
      <span className="text-sm text-zinc-600 dark:text-zinc-300">Looking it up</span>
      <span className="flex gap-1" aria-hidden>
        <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-violet-500 [animation-delay:-0.3s]" />
        <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-violet-500 [animation-delay:-0.15s]" />
        <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-violet-500" />
      </span>
      {seconds >= 3 && <span className="text-xs tabular-nums text-zinc-400">{seconds}s</span>}
    </div>
  )
}

function AnswerView({ answer }: { answer: AssistantAnswer }) {
  const looked = Array.from(new Set(answer.looked_at.map(lookedAtLabel)))
  // Each plan the answer proposed, as a card to check and create from.
  const plans = Array.from(new Set(planIds(answer)))
  // Each workflow the answer drafted, as a card to test and turn on.
  const workflows = Array.from(new Set(workflowIds(answer)))
  // Each dashboard the answer drafted, as a card to preview and save.
  const dashboards = Array.from(new Set(dashboardProposalIds(answer)))
  // Each change the answer proposed to a saved workflow, as a card to apply.
  const changes = Array.from(new Set(workflowChangeIds(answer)))
  // Each change the answer proposed to one dashboard tile, as a card to apply.
  const tileChanges = Array.from(new Set(dashboardChangeIds(answer)))
  // Each model's drafted data checks, as a card that opens them on its Checks tab.
  const checkDrafts = checksDrafts(answer)
  // The SQL each generate_sql call wrote, as a card to copy or open in the Explorer.
  const sql = sqlDrafts(answer)
  return (
    <div className="flex gap-2.5">
      <AssistantLogo />
      <div className="min-w-0 flex-1 space-y-2">
        <div className="text-sm text-zinc-900 dark:text-zinc-100">
          <AssistantMarkdown content={answer.answer} />
        </div>
        {plans.map((id) => (
          <PipelinePlanReview key={id} planId={id} compact />
        ))}
        {workflows.map((id) => (
          <WorkflowReview key={id} workflowId={id} />
        ))}
        {changes.map((id) => (
          <WorkflowChangeReview key={id} changeId={id} />
        ))}
        {dashboards.map((id) => (
          <DashboardProposalReview key={id} proposalId={id} />
        ))}
        {tileChanges.map((id) => (
          <DashboardChangeReview key={id} changeId={id} />
        ))}
        {checkDrafts.map((d) => (
          <ModelChecksDraftReview key={d.modelId} draft={d} />
        ))}
        {sql.map((d) => (
          <SqlDraftCard key={d.sql} draft={d} />
        ))}
        {answer.stopped === "limit" && (
          <p className="text-[11px] text-amber-700 dark:text-amber-300">
            Stopped at the limit for one question. Ask a narrower one to go further.
          </p>
        )}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-zinc-500 dark:text-zinc-400">
          {looked.length > 0 && <span>Looked at {looked.join(", ")}</span>}
          {answer.model?.label && (
            <span>
              Answered by {answer.model.label}
              {answer.model.model ? ` · ${answer.model.model}` : ""}
            </span>
          )}
          <CopyAnswer text={answer.answer} />
        </div>
      </div>
    </div>
  )
}

/** The plans an answer's plan_pipeline calls stored; a refused call stores none. */
export function planIds(answer: AssistantAnswer): string[] {
  return answer.looked_at
    .filter((l) => l.tool === "plan_pipeline" && l.ok && typeof l.resource_id === "string" && l.resource_id)
    .map((l) => l.resource_id as string)
}

/** The drafts an answer's plan_dashboard calls stored; a refused call stores none. */
export function dashboardProposalIds(answer: AssistantAnswer): string[] {
  return answer.looked_at
    .filter((l) => l.tool === "plan_dashboard" && l.ok && typeof l.resource_id === "string" && l.resource_id)
    .map((l) => l.resource_id as string)
}

/** The changes an answer's plan_dashboard_change calls stored; a refused call stores none. */
export function dashboardChangeIds(answer: AssistantAnswer): string[] {
  return answer.looked_at
    .filter((l) => l.tool === "plan_dashboard_change" && l.ok && typeof l.resource_id === "string" && l.resource_id)
    .map((l) => l.resource_id as string)
}

/** The changes an answer's plan_workflow_change calls stored; a refused call stores none. */
export function workflowChangeIds(answer: AssistantAnswer): string[] {
  return answer.looked_at
    .filter((l) => l.tool === "plan_workflow_change" && l.ok && typeof l.resource_id === "string" && l.resource_id)
    .map((l) => l.resource_id as string)
}

/**
 * The checks an answer's plan_model_checks calls drafted, one per model: a later draft for
 * a model replaces an earlier one. A refused call drafts none.
 */
export function checksDrafts(answer: AssistantAnswer): AssistantChecksDraft[] {
  const byModel = new Map<string, AssistantChecksDraft>()
  for (const l of answer.looked_at) {
    if (l.tool !== "plan_model_checks" || !l.ok) continue
    const d = draftsFromAssistant(l.arguments)
    if (!d) continue
    byModel.delete(d.modelId)
    byModel.set(d.modelId, d)
  }
  return Array.from(byModel.values())
}

/** The SQL an answer's generate_sql calls wrote, once each; a refused call wrote none. */
export function sqlDrafts(answer: AssistantAnswer): SqlDraft[] {
  const bySQL = new Map<string, SqlDraft>()
  for (const l of answer.looked_at) {
    if (l.tool !== "generate_sql" || !l.ok || typeof l.sql?.sql !== "string" || !l.sql.sql.trim()) continue
    bySQL.set(l.sql.sql, {
      sql: l.sql.sql,
      connection_id: typeof l.sql.connection_id === "string" ? l.sql.connection_id : "",
      tables: Array.isArray(l.sql.tables) ? l.sql.tables.filter((t) => typeof t === "string") : [],
    })
  }
  return Array.from(bySQL.values())
}

function CopyAnswer({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(timer)
  }, [copied])
  return (
    <button
      type="button"
      onClick={() => {
        navigator.clipboard?.writeText(text).then(
          () => setCopied(true),
          () => {},
        )
      }}
      className="inline-flex items-center gap-1 rounded px-1 py-0.5 hover:bg-zinc-100 hover:text-zinc-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
      aria-label={copied ? "Copied" : "Copy answer"}
    >
      {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
      {copied ? "Copied" : "Copy"}
    </button>
  )
}
