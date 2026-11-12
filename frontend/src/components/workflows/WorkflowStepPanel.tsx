"use client"

import Link from "next/link"

import { StatusPill, type PillTone } from "@/components/kit/StatusPill"
import { describeCron } from "@/components/explorer/cronSentence"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { formatDurationBetween } from "@/lib/duration"
import { cn } from "@/lib/utils"
import {
  DEFAULT_STEP_TIMEOUT_MINUTES,
  queryStep,
  type TriggerType,
  type Workflow,
  type WorkflowRun,
  type WorkflowStepRun,
} from "@/lib/api/workflows"
import { lastRunDelivery, type WorkflowStep } from "@/components/workflows/WorkflowCanvas"
import { WorkflowStepForm } from "@/components/workflows/WorkflowStepForm"
import { conditionLabel, draftRunNote, LINK_SLOT_NOTE, minutesWords, stepSentence } from "@/components/workflows/workflowWords"
import type { WorkflowDraft, WorkflowEditOptions } from "@/components/workflows/useWorkflowEdit"

const label = "text-[11px] font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400"
// No height cap: the panel scrolls, and its width is the user's (WorkflowPanelResizeHandle).
const code =
  "overflow-auto whitespace-pre-wrap rounded-md bg-zinc-50 p-2 font-mono text-[11px] text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200"

const link = "text-violet-700 hover:underline dark:text-violet-300"
const muted = "text-xs text-zinc-500 dark:text-zinc-400"
const facts = "grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1.5 text-xs"

/** True for a pipeline, model, wait, chart or dashboard node: read-only here, changed only by asking rsync.ai. */
function isAskOnlyNode(s: WorkflowStep): boolean {
  return s.kind === "run_pipeline" || s.kind === "refresh_model" || s.kind === "wait" || s.kind === "chart" || s.kind === "dashboard"
}

/**
 * One step's settings, beside the canvas, the way n8n opens a node. The step
 * buttons at the top are the keyboard path to every step, a channel's named
 * after its condition ("If → #ops", or "Condition 2 → #ops" of several), or
 * "Message → #ops" for a send that follows no condition; clicking a node on
 * the canvas selects the same one. The settings open with one sentence saying what the node does; with edit
 * set, they are fields, except a pipeline, model, wait, chart or dashboard step's, which
 * only Ask rsync.ai changes. Out of edit, the step's result in the last run
 * follows its settings.
 */
export function WorkflowStepPanel({
  workflow: w,
  steps,
  selectedId,
  onSelect,
  edit,
}: {
  workflow: Workflow
  steps: WorkflowStep[]
  selectedId: string
  onSelect: (id: string) => void
  edit?: { draft: WorkflowDraft; onChange: (next: WorkflowDraft) => void; options: WorkflowEditOptions }
}) {
  const step = steps.find((s) => s.id === selectedId) ?? steps[0]
  const d = w.definition
  const def = step.stepId ? d.steps.find((s) => s.id === step.stepId) : undefined
  const ref = step.stepId ? w.step_refs?.[step.stepId] : undefined
  // A channel of one of several conditions names it as runs, Details and Test do: "Condition 2", "Otherwise".
  const several = steps.filter((x) => x.kind === "condition").length > 1
  const conditionTitle = (i?: number) =>
    i === undefined
      ? "Message"
      : several
        ? conditionLabel(d, i)
        : steps.find((x) => x.kind === "condition" && x.branch === i)?.title
  const timeout = def ? minutesWords(def.timeout_minutes || DEFAULT_STEP_TIMEOUT_MINUTES) : ""
  // A condition shows its quiet time only; its rule is the sentence above.
  const hasSettings =
    !!edit || (step.kind === "condition" ? !!d.quiet_minutes : ["trigger", "query", "channel"].includes(step.kind) || !!def)
  return (
    <aside aria-label="Step settings" className="flex h-full min-h-0 flex-col gap-3 overflow-y-auto p-4 text-sm">
      <nav aria-label="Steps" className="flex flex-wrap gap-1">
        {steps.map((s, i) => (
          <button
            key={s.id}
            type="button"
            aria-pressed={s.id === step.id}
            onClick={() => onSelect(s.id)}
            className={cn(
              "rounded-md border px-2 py-0.5 text-xs",
              s.id === step.id
                ? "border-violet-400 bg-violet-50 text-violet-800 dark:border-violet-700 dark:bg-violet-950/50 dark:text-violet-200"
                : "border-zinc-200 text-zinc-600 hover:bg-zinc-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800",
            )}
          >
            {i + 1}. {s.kind === "channel" ? `${conditionTitle(s.branch) ?? ""} → ${s.title}` : s.title}
          </button>
        ))}
      </nav>

      <div>
        <h3 className="font-semibold text-zinc-900 dark:text-white">{step.title}</h3>
        <p className="text-xs text-zinc-600 dark:text-zinc-300">{stepSentence(w, step)}</p>
      </div>

      {hasSettings && (
        <PanelSection title="Settings">
          {edit && !isAskOnlyNode(step) && (
            <WorkflowStepForm
              step={step}
              draft={edit.draft}
              onChange={edit.onChange}
              options={edit.options}
              onSelect={onSelect}
            />
          )}
          {edit && isAskOnlyNode(step) && (
            <p className="text-xs text-zinc-500 dark:text-zinc-400">Only Ask rsync.ai changes this step.</p>
          )}

          {step.kind === "run_pipeline" && def && (
            <dl className={facts}>
              <dt className={label}>Pipeline</dt>
              <dd>
                {ref ? (
                  <Link href={`/pipelines/${ref.id}`} className={link}>
                    {ref.name}
                  </Link>
                ) : (
                  <span className="text-zinc-600 dark:text-zinc-300">Deleted</span>
                )}
              </dd>
              <dt className={label}>Timeout</dt>
              <dd className="text-zinc-800 dark:text-zinc-200">
                {timeout}
                {(def.retries ?? 0) > 0 && ", retries included"}
              </dd>
              <dt className={label}>Retries</dt>
              <dd className="text-zinc-800 dark:text-zinc-200">{def.retries ? def.retries : "None"}</dd>
            </dl>
          )}

          {step.kind === "refresh_model" && def && (
            <dl className={facts}>
              <dt className={label}>Model</dt>
              <dd>
                {ref ? (
                  <Link href={`/models/${ref.id}`} className={link}>
                    {ref.name}
                  </Link>
                ) : (
                  <span className="text-zinc-600 dark:text-zinc-300">Deleted</span>
                )}
              </dd>
              <dt className={label}>Timeout</dt>
              <dd className="text-zinc-800 dark:text-zinc-200">{timeout}</dd>
            </dl>
          )}

          {step.kind === "chart" && def && (
            <dl className={facts}>
              <dt className={label}>Chart</dt>
              <dd>
                {ref ? (
                  <Link href={`/charts/${ref.id}`} className={link}>
                    {ref.name}
                  </Link>
                ) : (
                  <span className="text-zinc-600 dark:text-zinc-300">Deleted</span>
                )}
              </dd>
            </dl>
          )}

          {step.kind === "dashboard" && def && (
            <dl className={facts}>
              <dt className={label}>Dashboard</dt>
              <dd>
                {ref ? (
                  <Link href={`/dashboards/${ref.id}`} className={link}>
                    {ref.name}
                  </Link>
                ) : (
                  <span className="text-zinc-600 dark:text-zinc-300">Deleted</span>
                )}
              </dd>
            </dl>
          )}

          {step.kind === "wait" && def && (
            <dl className={facts}>
              <dt className={label}>Waits</dt>
              <dd className="text-zinc-800 dark:text-zinc-200">{minutesWords(def.minutes ?? 0)}</dd>
            </dl>
          )}

          {!edit && step.kind === "trigger" && (
            <dl className={facts}>
              {d.trigger.type === "schedule" && (
                <>
                  {/* The sentence the edit form's cron note says, then the cron itself. */}
                  <dt className={label}>Runs</dt>
                  <dd className="text-zinc-800 dark:text-zinc-200">{describeCron(d.trigger.cron) ?? "On a custom schedule"}</dd>
                  <dt className={label}>Cron</dt>
                  <dd className="font-mono text-zinc-600 dark:text-zinc-300">{d.trigger.cron}</dd>
                  <dt className={label}>Time zone</dt>
                  <dd className="text-zinc-800 dark:text-zinc-200">{d.trigger.timezone || "UTC"}</dd>
                  <dt className={label}>Next run</dt>
                  <dd className="text-zinc-800 dark:text-zinc-200">
                    {w.status === "active" && w.next_run_at ? <LocalDateTime value={w.next_run_at} /> : "When it is turned on"}
                  </dd>
                </>
              )}
              {d.trigger.type === "after_pipeline" && (
                <>
                  <dt className={label}>Pipeline</dt>
                  <dd>
                    {w.pipeline ? (
                      <Link href={`/pipelines/${w.pipeline.id}`} className={link}>
                        {w.pipeline.name}
                      </Link>
                    ) : (
                      <span className="text-zinc-600 dark:text-zinc-300">Deleted</span>
                    )}
                  </dd>
                  <dt className={label}>Runs</dt>
                  <dd className="text-zinc-800 dark:text-zinc-200">Each time it finishes, once per run</dd>
                </>
              )}
              {d.trigger.type === "manual" && (
                <>
                  <dt className={label}>Runs</dt>
                  <dd className="text-zinc-800 dark:text-zinc-200">Only when someone clicks Run now</dd>
                </>
              )}
            </dl>
          )}

          {!edit && step.kind === "query" && (
            <div className="space-y-2 text-xs">
              <p className="text-zinc-600 dark:text-zinc-300">
                {w.connection ? `${w.connection.name} (${w.connection.connector_type ?? "connection"})` : "The connection was deleted."}{" "}
                One read-only SELECT, checked again on every run; up to 500 rows, with personal columns masked by name.
              </p>
              <pre aria-label="SQL" className={code}>
                {queryStep(d)?.sql}
              </pre>
            </div>
          )}

          {!edit && step.kind === "condition" && d.quiet_minutes ? (
            <p className="text-xs text-zinc-600 dark:text-zinc-300">
              After it sends, it waits {minutesWords(d.quiet_minutes)} before it sends again.
            </p>
          ) : null}

          {!edit && step.kind === "channel" && (
            <div className="space-y-2 text-xs">
              <p className="text-zinc-600 dark:text-zinc-300">
                {step.channelKind === "slack" ? "Posts to a Slack channel" : "Emails a list of addresses"}. Channels are
                managed in{" "}
                <Link href="/workspace/settings?tab=channels" className={link}>
                  Delivery channels
                </Link>
                .
              </p>
              {def?.message && (
                <div aria-label="Message" className="space-y-1 rounded-md bg-zinc-50 p-2 dark:bg-zinc-950">
                  {def.message.subject && (
                    <p className="font-medium text-zinc-800 dark:text-zinc-100">{def.message.subject}</p>
                  )}
                  <pre className="whitespace-pre-wrap font-sans text-zinc-700 dark:text-zinc-300">{def.message.body}</pre>
                </div>
              )}
              <p className="text-zinc-500 dark:text-zinc-400">
                {step.branch === undefined ? (
                  <>
                    It reads no query, so its only slots are {"{{workflow_name}}"}, {"{{run_time}}"} and {"{{link}}"}.{" "}
                    {LINK_SLOT_NOTE} Test shows the filled message.
                  </>
                ) : (
                  <>
                    Slots in {"{{ }}"} are filled from the query&apos;s first row when it runs; {"{{table}}"} adds the
                    rows. {LINK_SLOT_NOTE} Test shows the filled message.
                  </>
                )}
              </p>
            </div>
          )}
        </PanelSection>
      )}
      {!edit && <StepLastRun workflow={w} step={step} />}
    </aside>
  )
}

/** One titled part of the panel: the step's settings, or its last run. */
function PanelSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section aria-label={title} className="space-y-2 border-t border-zinc-100 pt-3 dark:border-zinc-800">
      <h4 className="text-xs font-semibold uppercase tracking-wide text-zinc-700 dark:text-zinc-200">{title}</h4>
      {children}
    </section>
  )
}

const STARTED_BY: Record<TriggerType, string> = {
  schedule: "Its schedule",
  after_pipeline: "The pipeline finishing",
  manual: "Run now",
}

/**
 * How the step went in the last run, in the words the canvas mark stands for: a
 * condition was true or not, a channel was sent to or not. Null when the run did
 * not get to it.
 */
function stepOutcome(step: WorkflowStep, run: WorkflowRun, sr?: WorkflowStepRun): { tone: PillTone; words: string } | null {
  switch (step.state) {
    case "ok": {
      // The trigger fired; how the run as a whole went is the other steps' marks.
      if (step.kind === "trigger") return { tone: "ok", words: "Triggered" }
      if (step.kind === "channel") return { tone: "ok", words: "Sent" }
      if (step.kind !== "condition") return { tone: "ok", words: "Succeeded" }
      return { tone: "ok", words: "True" }
    }
    case "untrue":
      return { tone: "skip", words: "Not true" }
    case "warned":
      return { tone: "wait", words: "Succeeded with a warning" }
    case "failed":
      return { tone: "fail", words: step.kind === "channel" ? "Not sent: failed" : "Failed" }
    case "skipped":
      return { tone: "skip", words: step.kind === "condition" ? "Not checked" : step.kind === "channel" ? "Not sent" : "Skipped" }
    case "running":
      return { tone: "run", words: sr?.status === "waiting" ? "Waiting" : "Running" }
    case "stopped":
      return { tone: "cancel", words: "Stopped" }
    default:
      return null
  }
}

/**
 * The step in the workflow's last run: how it went, when, how long it took, what
 * it started, and its error. A draft runs only when someone clicks Run now, and that
 * run is real: its steps ran and its messages went out. Its outcome keeps its colour,
 * as on the canvas, and a line under it says so.
 */
function StepLastRun({ workflow: w, step }: { workflow: Workflow; step: WorkflowStep }) {
  const run = w.last_run
  if (!run) {
    return (
      <PanelSection title="Last run">
        <p className={muted}>Not run yet.</p>
      </PanelSection>
    )
  }
  const draft = w.status === "draft"
  const sr = step.stepId ? run.steps?.find((x) => x.step_id === step.stepId) : undefined
  const outcome = stepOutcome(step, run, sr)
  if (!outcome) {
    return (
      <PanelSection title="Last run">
        <p className={muted}>
          {run.status === "running"
            ? "Not started yet."
            : // A step added since may simply not have been in what ran.
              run.earlier_version
              ? "Not in the last run, which used an earlier version of the workflow."
              : "Not reached in the last run."}
        </p>
      </PanelSection>
    )
  }
  // A send step fails as a whole when any of its channels does; a channel node says
  // its own delivery's error, and the step's only when it has no delivery of its own.
  const delivery = lastRunDelivery(run, step)
  const error =
    step.kind === "channel"
      ? delivery
        ? delivery.error
        : sr?.error
      : (sr?.error ?? (step.kind === "query" && step.state === "failed" ? run.error : undefined))
  const rows = step.kind === "query" ? (sr?.row_count ?? run.row_count) : sr?.row_count
  const startedAt = step.kind === "trigger" ? run.started_at : (sr?.started_at ?? run.started_at)
  return (
    <PanelSection title="Last run">
      <StatusPill tone={outcome.tone} title={draft ? draftRunNote(w) : undefined}>
        {outcome.words}
      </StatusPill>
      <dl className={facts}>
        <dt className={label}>Started</dt>
        <dd className="text-zinc-800 dark:text-zinc-200">
          <LocalDateTime value={startedAt} />
        </dd>
        {step.kind === "trigger" && (
          <>
            <dt className={label}>Started by</dt>
            <dd className="text-zinc-800 dark:text-zinc-200">{STARTED_BY[run.trigger] ?? run.trigger}</dd>
          </>
        )}
        {sr?.finished_at && (
          <>
            <dt className={label}>Took</dt>
            <dd className="text-zinc-800 dark:text-zinc-200">{formatDurationBetween(sr.started_at, sr.finished_at)}</dd>
          </>
        )}
        {sr && sr.attempt > 1 && (
          <>
            <dt className={label}>Attempt</dt>
            <dd className="text-zinc-800 dark:text-zinc-200">{sr.attempt}</dd>
          </>
        )}
        {rows !== undefined && rows !== null && (
          <>
            <dt className={label}>Rows</dt>
            <dd className="text-zinc-800 dark:text-zinc-200">{rows.toLocaleString()}</dd>
          </>
        )}
      </dl>
      {sr?.execution_id && (
        <Link href={`/executions/${sr.execution_id}`} className={cn(link, "block text-xs")}>
          Open this pipeline run
        </Link>
      )}
      {sr?.model_id && (
        <Link href={`/models/${sr.model_id}`} className={cn(link, "block text-xs")}>
          Open the model&apos;s runs
        </Link>
      )}
      {error && (
        <p className="whitespace-pre-wrap rounded-md bg-red-50 p-2 text-xs text-red-700 dark:bg-red-950/40 dark:text-red-300">
          {error}
        </p>
      )}
      {sr?.note && <p className={muted}>{sr.note}</p>}
      {draft && <p className={muted}>{draftRunNote(w)}</p>}
      {run.earlier_version && <p className={muted}>This run used an earlier version of the workflow.</p>}
    </PanelSection>
  )
}
