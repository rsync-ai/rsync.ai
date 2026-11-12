"use client"

import { useState, type ReactNode } from "react"
import Link from "next/link"

import { describeCron } from "@/components/explorer/cronSentence"
import type { WorkflowStep } from "@/components/workflows/WorkflowCanvas"
import type { WorkflowDraft, WorkflowEditOptions } from "@/components/workflows/useWorkflowEdit"
import { conditionLabel, LINK_SLOT_NOTE, OP_WORDS } from "@/components/workflows/workflowWords"
import { timeZoneChoices } from "@/lib/timezones"
import {
  addCondition,
  caseSend,
  conditionStep,
  definitionQuery,
  isBuiltShape,
  moveCondition,
  removeCondition,
  withBranch,
  withQuery,
  type ConditionType,
  type TriggerType,
  type WorkflowCondition,
  type WorkflowDefinition,
  type WorkflowMessage,
} from "@/lib/api/workflows"

// The server's limits (internal/workflow/definition.go); it refuses past them.
const MAX_SQL = 8000
const MAX_SUBJECT = 200
const MAX_BODY = 4000
const MAX_QUIET_MINUTES = 7 * 24 * 60
/** Per send. */
const MAX_CHANNELS = 5

const label = "text-[11px] font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400"
const field =
  "w-full rounded-md border border-zinc-300 bg-white px-2 py-1.5 text-xs text-zinc-900 focus:outline-none focus:ring-2 focus:ring-violet-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
const hint = "text-[11px] text-zinc-500 dark:text-zinc-400"
const smallButton =
  "rounded-md border border-zinc-300 px-2 py-1 text-xs font-medium text-zinc-700 hover:bg-zinc-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"

function Field({ name, children, note }: { name: string; children: ReactNode; note?: ReactNode }) {
  return (
    <label className="block space-y-1">
      <span className={label}>{name}</span>
      {children}
      {note && <span className={`block ${hint}`}>{note}</span>}
    </label>
  )
}

/** A number box that lets the person clear it and type a minus sign before the number is whole. */
function NumberInput({
  value,
  onChange,
  ariaLabel,
  min,
  max,
}: {
  value: number
  onChange: (n: number) => void
  ariaLabel?: string
  min?: number
  max?: number
}) {
  const [text, setText] = useState(String(value))
  // Only an outside change of value rewrites the text (adjusting state while rendering).
  const [seen, setSeen] = useState(value)
  if (seen !== value) {
    setSeen(value)
    if (Number(text) !== value) setText(String(value))
  }
  return (
    <input
      type="number"
      inputMode="decimal"
      aria-label={ariaLabel}
      min={min}
      max={max}
      className={field}
      value={text}
      onChange={(e) => {
        setText(e.target.value)
        const n = Number(e.target.value)
        if (e.target.value.trim() !== "" && Number.isFinite(n)) onChange(n)
      }}
    />
  )
}

/**
 * A step's settings as fields, when an owner or admin edits the workflow. The
 * server checks the whole draft on Save (workflow.Normalize, then the query
 * once), so these fields only keep it within the server's limits; a refusal
 * names the field and the editor opens its step.
 *
 * A condition node is one condition's (step.branch, 0-based; the API calls a
 * condition and its message a branch); a channel node is one send step's
 * (step.stepId), a condition's or one that follows no condition. A condition's
 * test, a send's message and its channels are edited here. On a workflow of
 * only a query, its conditions and their messages (isBuiltShape), a condition
 * is also added, removed and moved up or down here; onSelect then opens the
 * condition that moved or was added. Adding or removing any other step, and a
 * pipeline, model or wait step's settings, only Ask rsync.ai changes.
 */
export function WorkflowStepForm({
  step,
  draft,
  onChange,
  options,
  onSelect,
}: {
  step: WorkflowStep
  draft: WorkflowDraft
  onChange: (next: WorkflowDraft) => void
  options: WorkflowEditOptions
  onSelect?: (id: string) => void
}) {
  const d = draft.definition
  const kind = step.kind
  const setDef = (next: Partial<WorkflowDefinition>) => onChange({ ...draft, definition: { ...d, ...next } })
  const setWhole = (definition: WorkflowDefinition) => onChange({ ...draft, definition })

  if (kind === "trigger") {
    const cronWords = d.trigger.type === "schedule" ? describeCron(d.trigger.cron) : null
    return (
      <div className="space-y-3 text-xs">
        <Field name="Runs">
          <select
            className={field}
            value={d.trigger.type}
            onChange={(e) => setDef({ trigger: { ...d.trigger, type: e.target.value as TriggerType } })}
          >
            <option value="schedule">On a schedule</option>
            <option value="after_pipeline">After a pipeline finishes</option>
            <option value="manual">Only when someone clicks Run now</option>
          </select>
        </Field>
        {d.trigger.type === "schedule" && (
          <>
            <Field
              name="Cron"
              note={
                cronWords ??
                "Five fields: minute, hour, day of month, month, day of week. 0 9 * * 1 is every Monday at 09:00."
              }
            >
              <input
                className={`${field} font-mono`}
                value={d.trigger.cron ?? ""}
                placeholder="0 9 * * *"
                spellCheck={false}
                onChange={(e) => setDef({ trigger: { ...d.trigger, cron: e.target.value } })}
              />
            </Field>
            <Field name="Time zone">
              <input
                className={field}
                value={d.trigger.timezone ?? ""}
                placeholder="UTC"
                list="workflow-time-zones"
                spellCheck={false}
                onChange={(e) => setDef({ trigger: { ...d.trigger, timezone: e.target.value } })}
              />
              <datalist id="workflow-time-zones">
                {timeZoneChoices().map((tz) => (
                  <option key={tz} value={tz} />
                ))}
              </datalist>
            </Field>
          </>
        )}
        {d.trigger.type === "after_pipeline" && (
          <Field name="Pipeline" note="It runs each time this pipeline finishes, once per run.">
            <select
              className={field}
              value={d.trigger.pipeline_id ?? ""}
              onChange={(e) => setDef({ trigger: { ...d.trigger, pipeline_id: e.target.value } })}
            >
              <option value="">Pick a pipeline</option>
              {options.pipelines.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        {d.trigger.type === "manual" && <p className={hint}>It runs only when someone clicks Run now.</p>}
      </div>
    )
  }

  if (kind === "query") {
    const q = definitionQuery(d) ?? { connection_id: "", sql: "" }
    return (
      <div className="space-y-3 text-xs">
        <Field name="Connection" note="A connection the Data Explorer queries with SQL.">
          <select
            className={field}
            value={q.connection_id}
            onChange={(e) => setWhole(withQuery(d, { ...q, connection_id: e.target.value }))}
          >
            <option value="">Pick a connection</option>
            {options.connections.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
                {c.connector_type ? ` (${c.connector_type})` : ""}
              </option>
            ))}
          </select>
        </Field>
        {options.loaded && options.connections.length === 0 && (
          <p className={hint}>
            No SQL connections to pick from.{" "}
            <Link href="/connections" className="text-violet-700 hover:underline dark:text-violet-300">
              Add one in Connections
            </Link>
            .
          </p>
        )}
        <Field
          name="SQL"
          note="One read-only SELECT; up to 500 rows, with personal columns masked by name. Save runs it once to check it and sends nothing."
        >
          <textarea
            aria-label="SQL"
            className={`${field} min-h-[180px] font-mono`}
            value={q.sql}
            maxLength={MAX_SQL}
            spellCheck={false}
            onChange={(e) => setWhole(withQuery(d, { ...q, sql: e.target.value }))}
          />
        </Field>
      </div>
    )
  }

  if (kind === "condition") {
    const conds = conditionStep(d)?.conditions ?? []
    const n = conds.length
    const branch = step.branch ?? 0
    const c = conds[branch]
    const send = caseSend(d, branch)
    if (!c || !send) return null
    const setCondition = (condition: WorkflowCondition) => setWhole(withBranch(d, send.id, { condition }))
    const last = branch === n - 1
    const built = isBuiltShape(d)
    const add = () => {
      const { definition, at, send: added } = addCondition(d)
      onChange({ ...draft, definition, nodeChannelIds: { ...draft.nodeChannelIds, [added]: [] } })
      onSelect?.(`branch:${at}`)
    }
    const remove = () => {
      const removed = removeCondition(d, branch)
      if (!removed) return
      const nodeChannelIds = { ...draft.nodeChannelIds }
      delete nodeChannelIds[removed.send]
      onChange({ ...draft, definition: removed.definition, nodeChannelIds })
      onSelect?.(`branch:${Math.min(branch, n - 2)}`)
    }
    const move = (to: number) => {
      const moved = moveCondition(d, branch, to)
      if (!moved) return
      setWhole(moved)
      onSelect?.(`branch:${to}`)
    }
    const up = branch > 0 ? moveCondition(d, branch, branch - 1) : null
    const down = branch < n - 1 ? moveCondition(d, branch, branch + 1) : null
    // Only the last condition may send always ("Otherwise" when there are several).
    return (
      <div className="space-y-3 text-xs">
        <Field
          name="Sends"
          note={
            n > 1
              ? built
                ? "The first condition that is true sends, to its own channels. Move a condition up to check it first."
                : "The first condition that is true sends, to its own channels. Only Ask rsync.ai adds or removes a condition on a workflow with pipeline, model, wait, chart or dashboard steps."
              : built
                ? "To send different messages for different results, add a condition."
                : "Only Ask rsync.ai adds a condition to a workflow with pipeline, model, wait, chart or dashboard steps."
          }
        >
          <select
            className={field}
            value={c.type}
            onChange={(e) => {
              const type = e.target.value as ConditionType
              setCondition({ ...c, type, op: type === "value" ? (c.op ?? ">") : c.op })
            }}
          >
            {last && (
              <option value="always">{n > 1 ? "Otherwise (when no condition above is true)" : "Every time it runs"}</option>
            )}
            <option value="rows_exist">When the query returns rows</option>
            <option value="no_rows">When the query returns no rows</option>
            <option value="value">When a value passes a test</option>
          </select>
        </Field>
        {c.type === "value" && (
          <>
            <Field name="Column" note="Read from the first row. Empty means the first column.">
              <input
                className={`${field} font-mono`}
                value={c.column ?? ""}
                placeholder="first column"
                spellCheck={false}
                onChange={(e) => setCondition({ ...c, column: e.target.value })}
              />
            </Field>
            <div className="grid grid-cols-[max-content_1fr] gap-2">
              <Field name="Test">
                <select
                  className={field}
                  value={c.op ?? ">"}
                  onChange={(e) => setCondition({ ...c, op: e.target.value })}
                >
                  {[">", ">=", "<", "<=", "=", "!="].map((op) => (
                    <option key={op} value={op}>
                      {OP_WORDS[op]}
                    </option>
                  ))}
                </select>
              </Field>
              <Field name="Threshold">
                <NumberInput
                  value={c.threshold}
                  onChange={(t) => setCondition({ ...c, threshold: t })}
                />
              </Field>
            </div>
          </>
        )}
        <Field
          name="Quiet minutes"
          note={
            n > 1
              ? "For every condition: after a condition sends, it waits this long before it sends again. 0 sends every time it is true."
              : "After a send, wait this long before sending again. 0 sends every time it is true."
          }
        >
          <NumberInput
            value={d.quiet_minutes ?? 0}
            min={0}
            max={MAX_QUIET_MINUTES}
            onChange={(n) => setDef({ quiet_minutes: n })}
          />
        </Field>
        {built && (
          <div role="group" aria-label="Conditions" className="flex flex-wrap gap-1.5 border-t border-zinc-100 pt-3 dark:border-zinc-800">
            <button type="button" className={smallButton} onClick={add}>
              Add a condition
            </button>
            {n > 1 && (
              <>
                <button type="button" className={smallButton} disabled={!up} onClick={() => move(branch - 1)}>
                  Move up
                </button>
                <button type="button" className={smallButton} disabled={!down} onClick={() => move(branch + 1)}>
                  Move down
                </button>
                <button type="button" className={`${smallButton} text-red-700 dark:text-red-300`} onClick={remove}>
                  Remove this condition
                </button>
              </>
            )}
          </div>
        )}
      </div>
    )
  }

  if (kind !== "channel") return null

  // A channel step: its send's message and the channels it goes to, the same for each of them.
  const send = step.stepId ? d.steps.find((s) => s.id === step.stepId && s.type === "send") : undefined
  if (!send) return null
  const k = send.case
  const n = conditionStep(d)?.conditions?.length ?? 0
  const ids = draft.nodeChannelIds[send.id] ?? []
  const picked = new Set(ids)
  const toggle = (id: string, on: boolean) =>
    onChange({
      ...draft,
      nodeChannelIds: { ...draft.nodeChannelIds, [send.id]: on ? [...ids, id] : ids.filter((x) => x !== id) },
    })
  const m: WorkflowMessage = send.message ?? { subject: "", body: "" }
  const setMessage = (message: WorkflowMessage) => setWhole(withBranch(d, send.id, { message }))
  return (
    <div className="space-y-3 text-xs">
      <fieldset className="space-y-1">
        <legend className={label}>{k !== undefined && n > 1 ? `${conditionLabel(d, k)}'s channels` : "Channels"}</legend>
        {options.loaded && options.channels.length === 0 ? (
          <p className={hint}>
            No delivery channels to pick from.{" "}
            <Link href="/workspace/settings?tab=channels" className="text-violet-700 hover:underline dark:text-violet-300">
              Add one in Delivery channels
            </Link>
            .
          </p>
        ) : (
          options.channels.map((ch) => {
            const on = picked.has(ch.id)
            return (
              <label key={ch.id} className="flex items-center gap-2 text-zinc-800 dark:text-zinc-200">
                <input
                  type="checkbox"
                  checked={on}
                  disabled={!on && picked.size >= MAX_CHANNELS}
                  onChange={(e) => toggle(ch.id, e.target.checked)}
                />
                {ch.name}
                <span className={hint}>{ch.kind === "slack" ? "Slack" : "Email"}</span>
              </label>
            )
          })
        )}
        <span className={`block ${hint}`}>Up to {MAX_CHANNELS}. Each one gets the same message.</span>
      </fieldset>
      <Field name="Subject" note="The email's subject, and the bold first line in Slack. Empty uses the workflow's name.">
        <input
          className={field}
          value={m.subject}
          maxLength={MAX_SUBJECT}
          onChange={(e) => setMessage({ ...m, subject: e.target.value })}
        />
      </Field>
      <Field
        name="Message"
        note={
          k === undefined ? (
            <>
              It reads no query, so its only slots are {"{{workflow_name}}"}, {"{{run_time}}"} and {"{{link}}"}.{" "}
              {LINK_SLOT_NOTE}
            </>
          ) : (
            <>
              Slots in {"{{ }}"} are filled from the query&apos;s first row when it runs; {"{{table}}"} adds the rows.{" "}
              {LINK_SLOT_NOTE}
            </>
          )
        }
      >
        <textarea
          className={`${field} min-h-[140px]`}
          value={m.body}
          maxLength={MAX_BODY}
          onChange={(e) => setMessage({ ...m, body: e.target.value })}
        />
      </Field>
    </div>
  )
}
