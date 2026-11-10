import type { WorkflowStep } from "@/components/workflows/WorkflowCanvas"
import {
  DEFAULT_STEP_TIMEOUT_MINUTES,
  caseSend,
  conditionStep,
  stepColumns,
  stepRule,
  stepsInOrder,
  type ConditionType,
  type Workflow,
  type WorkflowCondition,
  type WorkflowDefinition,
  type WorkflowStepDef,
} from "@/lib/api/workflows"

// A workflow in plain words. The API calls each condition and its message a
// "branch"; a person reads "condition", "is true", and "is above 5,000"
// rather than "> 5000". The server writes the same words for a condition
// (internal/workflow/definition.go: opWords, groupThousands, DescribeCondition).

/** What {{link}} opens, said wherever a message is written: a body that says
 * "open the dashboard" before it would send people to the workflow. */
export const LINK_SLOT_NOTE = "{{link}} opens this workflow's page, not a dashboard or chart."

/**
 * What a draft's last run was. A draft never runs on its own, so its runs are Run
 * now, and Run now is real whatever the status (workflows.go RunWorkflowNow): it
 * sends. Saying "a try" here once read as "nothing went out". A workflow with no
 * condition always sends, so its note does not mention one.
 */
export function draftRunNote(w: Workflow): string {
  const sent = conditions(w).length > 0 ? "its message was sent if a condition was true" : "its message was sent"
  return `A draft runs only when someone clicks Run now, and that run is real: its steps ran and ${sent}.`
}

/** A value condition's operator, spelled out. */
export const OP_WORDS: Record<string, string> = {
  ">": "is above",
  ">=": "is at least",
  "<": "is below",
  "<=": "is at most",
  "=": "is",
  "!=": "is not",
}

/** 1234567.5 → "1,234,567.5": commas in the whole part only. */
export function groupThousands(n: number): string {
  const m = /^(-?)(\d+)(.*)$/.exec(String(n))
  if (!m) return String(n)
  return m[1] + m[2].replace(/\B(?=(\d{3})+(?!\d))/g, ",") + m[3]
}

/** What must be true for a condition to send: "failed is above 5,000"; null for one that always sends. */
export function conditionClause(c: WorkflowCondition): string | null {
  switch (c.type) {
    case "rows_exist":
      return "the query returns rows"
    case "no_rows":
      return "the query returns no rows"
    case "value": {
      const op = c.op ?? ">"
      return `${c.column || "the first column"} ${OP_WORDS[op] ?? op} ${groupThousands(c.threshold)}`
    }
    default:
      return null
  }
}

/** A condition's node label: "If" for the first, "Otherwise" for a last "always" of several, else "Else if". */
export function branchTitle(i: number, n: number, type: ConditionType): string {
  if (i === 0) return "If"
  if (i === n - 1 && type === "always") return "Otherwise"
  return "Else if"
}

/** 30 → "30 minutes", 60 → "1 hour", 90 → "1 hour 30 minutes", 720 → "12 hours". */
export function minutesWords(m: number): string {
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`
  if (m < 60) return plural(m, "minute")
  const h = Math.floor(m / 60)
  const rest = m % 60
  return rest === 0 ? plural(h, "hour") : `${plural(h, "hour")} ${plural(rest, "minute")}`
}

/** A wait step named by its length: "the 10-minute wait", "the 6-hour wait", "the 1 hour 30 minutes wait". */
function waitName(m: number): string {
  if (m < 60) return `the ${m}-minute wait`
  if (m % 60 === 0) return `the ${m / 60}-hour wait`
  return `the ${minutesWords(m)} wait`
}

function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s
}

/** "#ops", "#ops and Finance list", "#ops, Finance list and Pager"; `or` for "a, b or c". */
function joinNames(names: string[], last = "and"): string {
  if (names.length <= 1) return names[0] ?? ""
  return `${names.slice(0, -1).join(", ")} ${last} ${names[names.length - 1]}`
}

/** The names of the channels a send step sends to. */
function sendChannelNames(w: Workflow, sendId: string | undefined): string[] {
  if (!sendId) return []
  return w.channels.filter((c) => c.node_id === sendId).map((c) => c.name)
}

function channelNames(w: Workflow, i: number): string[] {
  return sendChannelNames(w, caseSend(w.definition, i)?.id)
}

function conditions(w: Workflow): WorkflowCondition[] {
  return conditionStep(w.definition)?.conditions ?? []
}

/** The 0-based condition i is the last and always true: "Otherwise", which is no condition at all. */
export function isOtherwise(w: Workflow, i: number): boolean {
  return isOtherwiseIn(w.definition, i)
}

/** isOtherwise for a definition: a run's own, which may be an earlier version's. */
export function isOtherwiseIn(d: WorkflowDefinition, i: number): boolean {
  const cs = conditionStep(d)?.conditions ?? []
  return cs.length > 1 && i === cs.length - 1 && cs[i].type === "always"
}

/** Condition i of several by its name: "Otherwise", or "Condition 2". */
export function conditionLabel(d: WorkflowDefinition, i: number): string {
  return isOtherwiseIn(d, i) ? "Otherwise" : `Condition ${i + 1}`
}

/** A step as a sentence names it: "pipeline Orders", "model Revenue", "chart Sales", "dashboard Ops", "the query". */
export function stepName(w: Workflow, s: WorkflowStepDef): string {
  const ref = w.step_refs?.[s.id]
  switch (s.type) {
    case "run_pipeline":
      return ref ? `pipeline ${ref.name}` : "a deleted pipeline"
    case "refresh_model":
      return ref ? `model ${ref.name}` : "a deleted model"
    case "chart":
      return ref ? `chart ${ref.name}` : "a deleted chart"
    case "dashboard":
      return ref ? `dashboard ${ref.name}` : "a deleted dashboard"
    case "wait":
      return waitName(s.minutes ?? 0)
    case "query":
      return "the query"
    case "condition":
      return "the condition"
    default: {
      if (s.case !== undefined) {
        if (conditions(w).length <= 1) return "the message"
        return isOtherwise(w, s.case) ? "the Otherwise message" : `condition ${s.case + 1}'s message`
      }
      const to = joinNames(sendChannelNames(w, s.id))
      return to ? `the message to ${to}` : "a message"
    }
  }
}

/**
 * When a step runs, from its steps before it and its rule: "after pipeline
 * Orders succeeds", "when pipeline A or pipeline B fails".
 */
export function afterWords(w: Workflow, s: WorkflowStepDef): string {
  const parents = (s.after ?? [])
    .map((id) => w.definition.steps.find((p) => p.id === id))
    .filter((p): p is WorkflowStepDef => !!p)
    .map((p) => stepName(w, p))
  if (parents.length === 0) return "when the workflow starts"
  const one = parents.length === 1
  switch (stepRule(s)) {
    case "any_failed":
      return `when ${joinNames(parents, "or")} fails`
    case "all_done":
      return one
        ? `after ${parents[0]} finishes, even if it failed`
        : `after ${joinNames(parents)} all finish, even if any of them failed`
    default:
      return one ? `after ${parents[0]} succeeds` : `after ${joinNames(parents)} all succeed`
  }
}

/** What a pipeline, model, wait, chart, dashboard or query step does, as the workflow's summary says it. */
function actionWords(w: Workflow, s: WorkflowStepDef): string {
  let text: string
  switch (s.type) {
    case "run_pipeline":
    case "refresh_model": {
      const ref = w.step_refs?.[s.id]
      const verb = s.type === "run_pipeline" ? "runs" : "refreshes"
      const what = s.type === "run_pipeline" ? "pipeline" : "model"
      text = ref ? `${verb} ${what} ${ref.name}` : `${verb} a deleted ${what}`
      break
    }
    case "wait":
      text = `waits ${minutesWords(s.minutes ?? 0)}`
      break
    case "chart": {
      const ref = w.step_refs?.[s.id]
      text = ref ? `draws chart ${ref.name}` : "draws a deleted chart"
      break
    }
    case "dashboard": {
      const ref = w.step_refs?.[s.id]
      text = ref ? `draws dashboard ${ref.name}` : "draws a deleted dashboard"
      break
    }
    default: {
      // No connection id yet is one to pick (a new workflow), not a deleted one.
      const on = w.connection?.name ?? (s.connection_id ? "a deleted connection" : "a connection you pick")
      text = `runs one read-only query on ${on}`
    }
  }
  const rule = stepRule(s)
  if (rule === "any_failed") text += " (when a step before it fails)"
  else if (rule === "all_done") text += " (even if a step before it failed)"
  return text
}

/**
 * The whole workflow in a few lines, shown above the canvas: when it runs,
 * what it runs and reads, left to right, and, for several conditions, each
 * one in order with where it sends.
 */
export function workflowSummary(w: Workflow): { lead: string; conditions: string[] } {
  const d = w.definition
  const trigger = capitalize(w.trigger_description)
  const col = stepColumns(d)
  const ordered = stepsInOrder(d)
  const actions = ordered.filter((s) => s.type !== "condition" && s.type !== "send")
  const plain = ordered.filter((s) => s.type === "send" && s.case === undefined)

  if (actions.length === 0) {
    const to = joinNames(plain.flatMap((s) => sendChannelNames(w, s.id)))
    return { lead: to ? `${trigger}, rsync.ai sends a message to ${to}.` : `${trigger}, rsync.ai sends nothing.`, conditions: [] }
  }

  // Steps in one column run side by side ("and"); a later column runs after ("then").
  const columns: string[][] = []
  let last = -1
  for (const s of actions) {
    const c = col.get(s.id) ?? 1
    if (c !== last) columns.push([])
    last = c
    columns[columns.length - 1].push(actionWords(w, s))
  }
  let start = `${trigger}, rsync.ai ${columns.map((c) => c.join(" and ")).join(", then ")}`
  // A message sent once the last steps all succeed reads as the next "then"; it is
  // "it" after a chart or dashboard, since the message carries the drawing.
  const lastCol = Math.max(...actions.map((s) => col.get(s.id) ?? 1))
  const lastIds = actions.filter((s) => (col.get(s.id) ?? 1) === lastCol).map((s) => s.id)
  const follows = (s: WorkflowStepDef) =>
    stepRule(s) === "all_success" && s.after?.length === lastIds.length && lastIds.every((id) => s.after?.includes(id))
  const after = plain
    .map((s) => {
      const to = joinNames(sendChannelNames(w, s.id))
      if (!to) return ""
      if (follows(s)) {
        const drawn = lastIds.some((id) => ["chart", "dashboard"].includes(d.steps.find((x) => x.id === id)?.type ?? ""))
        start += `, then sends ${drawn ? "it" : "a message"} to ${to}`
        return ""
      }
      return ` It sends a message to ${to} ${afterWords(w, s)}.`
    })
    .join("")

  const cs = conditions(w)
  if (cs.length === 0) return { lead: `${start}.${after}`, conditions: [] }
  if (cs.length === 1) {
    const to = joinNames(channelNames(w, 0))
    const clause = conditionClause(cs[0])
    const sends = to ? `sends its message to ${to}` : "sends nothing: it has no channel"
    return { lead: (clause ? `${start} and, when ${clause}, ${sends}.` : `${start} and ${sends}.`) + after, conditions: [] }
  }
  return {
    lead:
      `${start} and checks its ${cs.length} conditions in order: only the first that is true sends its message, to its own channels.` +
      after,
    conditions: cs.map((c, i) => {
      const to = joinNames(channelNames(w, i)) || "no channel"
      if (isOtherwise(w, i)) return `Otherwise → ${to}`
      const title = branchTitle(i, cs.length, c.type)
      return `${title} ${conditionClause(c) ?? "always"} → ${to}`
    }),
  }
}

/** "It may take up to 12 hours, retries included." */
function timeoutWords(s: WorkflowStepDef, retries: boolean): string {
  const m = s.timeout_minutes || DEFAULT_STEP_TIMEOUT_MINUTES
  return `It may take up to ${minutesWords(m)}${retries ? ", retries included" : ""}.`
}

/** One sentence at the top of a node's settings, saying what that node does. */
export function stepSentence(w: Workflow, step: WorkflowStep): string {
  const cs = conditions(w)
  const n = cs.length
  const def = step.stepId ? w.definition.steps.find((s) => s.id === step.stepId) : undefined
  switch (step.kind) {
    case "trigger":
      return `Starts the workflow ${w.trigger_description}.`
    case "query": {
      const on = w.connection
        ? `on ${w.connection.name}`
        : def?.connection_id
          ? "on a connection that was deleted"
          : "on a connection you pick"
      // After other steps, say when, as each of them does: the summary already says it.
      const when = def?.after?.length ? ` ${afterWords(w, def)}` : ""
      if (n === 0) return `Runs one read-only query ${on}${when}.`
      const after = n > 1 ? `the ${n} conditions after it read its result` : "the condition after it reads its result"
      return `Runs one read-only query ${on}${when}; ${after}.`
    }
    case "run_pipeline": {
      if (!def) return step.subtitle
      const what = w.step_refs?.[def.id] ? `pipeline ${w.step_refs[def.id].name}` : "a pipeline that was deleted"
      const r = def.retries ?? 0
      const retries = r > 0 ? ` A failed run is started again, up to ${r} time${r === 1 ? "" : "s"}.` : ""
      return (
        `Runs ${what} ${afterWords(w, def)} and waits for it to finish; when the pipeline is already running, ` +
        `it waits for that run, then starts its own.${retries} ${timeoutWords(def, r > 0)}`
      )
    }
    case "refresh_model": {
      if (!def) return step.subtitle
      const what = w.step_refs?.[def.id] ? `model ${w.step_refs[def.id].name}` : "a model that was deleted"
      return (
        `Refreshes ${what} ${afterWords(w, def)}, with its checks: a failing Warn check marks the step warned, ` +
        `a failing Block check fails it. When the model is already refreshing, it waits, then starts its own refresh. ` +
        timeoutWords(def, false)
      )
    }
    case "wait":
      return def ? `Waits ${minutesWords(def.minutes ?? 0)} ${afterWords(w, def)}, then the steps after it start.` : step.subtitle
    case "chart": {
      if (!def) return step.subtitle
      const what = w.step_refs?.[def.id] ? `chart ${w.step_refs[def.id].name}` : "a chart that was deleted"
      return (
        `Draws ${what} ${afterWords(w, def)}, as its page shows it to the person the run acts as, for the message ` +
        `after it: email shows a bar, line, area or heatmap chart as a picture and a scorecard or table as its ` +
        `numbers; Slack gets its numbers and a link to the chart.`
      )
    }
    case "dashboard": {
      if (!def) return step.subtitle
      const what = w.step_refs?.[def.id] ? `dashboard ${w.step_refs[def.id].name}` : "a dashboard that was deleted"
      return (
        `Draws every tile of ${what} ${afterWords(w, def)}, as its page shows it to the person the run acts as, ` +
        `with no filters picked, for the message after it: email shows the tiles in the board's order, each chart ` +
        `as a picture and each scorecard or table as its numbers, and attaches the dashboard as a PDF; Slack gets ` +
        `its numbers and a link to the dashboard. A tile that cannot be drawn says so and the rest still go.`
      )
    }
  }

  // A channel of a send that follows no condition.
  if (step.kind === "channel" && step.branch === undefined) {
    if (step.id.endsWith(":none")) return "This message has no channel, so it sends nothing."
    const verb = step.channelKind === "slack" ? "Posts" : "Emails"
    return def ? `${verb} its message to ${step.title} ${afterWords(w, def)}.` : step.subtitle
  }

  const i = step.branch ?? 0
  const c = cs[i]
  if (!c) return step.subtitle
  const clause = conditionClause(c)
  const otherwise = isOtherwise(w, i)

  if (step.kind === "condition") {
    if (otherwise) return "Sends its message when no condition above is true."
    if (n === 1) return clause ? `Sends its message when ${clause}.` : "Sends its message every time it runs."
    const below = n - 1 - i
    const rest =
      below === 0 ? "" : below === 1 ? " and the condition below is not checked" : " and the conditions below are not checked"
    const lead = i === 0 ? "Checked first" : "Checked only when no condition above is true"
    return clause
      ? `${lead}: when ${clause}, it sends its message${rest}.`
      : `${lead}: it always sends its message${rest}.`
  }

  // A channel of a condition's send.
  const whose = n <= 1 ? "the message" : otherwise ? "the Otherwise message" : `condition ${i + 1}'s message`
  if (step.id.endsWith(":none")) {
    return `${n > 1 ? conditionLabel(w.definition, i) : "The condition"} has no channel, so it sends nothing.`
  }
  const verb = step.channelKind === "slack" ? "Posts" : "Emails"
  let when: string
  if (otherwise) when = "when no condition above is true"
  else if (!clause) when = i === 0 ? "every time it runs" : "when no condition above is true"
  else when = i === 0 ? `when ${clause}` : `when ${clause} and no condition above is true`
  return `${verb} ${whose} to ${step.title} ${when}.`
}
