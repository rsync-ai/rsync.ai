import Link from "next/link"

import { formatAbsoluteTime } from "@/lib/utils"
import { Card } from "@/components/ui/card"
import { DetailRow } from "@/components/ui/detail-row"
import { conditionLabel, isOtherwiseIn, minutesWords } from "@/components/workflows/workflowWords"
import {
  conditionStep,
  isProcessStep,
  queryStep,
  stepsInOrder,
  type Workflow,
  type WorkflowStepDef,
} from "@/lib/api/workflows"

const link = "text-violet-700 hover:underline dark:text-violet-300"

/** A pipeline, model, wait, chart or dashboard step as the Details card lists it, with a link to what it runs or draws. */
function ProcessStepLine({ workflow: w, step: s }: { workflow: Workflow; step: WorkflowStepDef }) {
  const ref = w.step_refs?.[s.id]
  if (s.type === "wait") return <div>{`Wait ${minutesWords(s.minutes ?? 0)}`}</div>
  const [what, href] =
    s.type === "run_pipeline"
      ? ["Run pipeline", `/pipelines/${ref?.id}`]
      : s.type === "chart"
        ? ["Draw chart", `/charts/${ref?.id}`]
        : s.type === "dashboard"
          ? ["Draw dashboard", `/dashboards/${ref?.id}`]
          : ["Refresh model", `/models/${ref?.id}`]
  return (
    <div className="break-words">
      {what}{" "}
      {ref ? (
        <Link href={href} className={link}>
          {ref.name}
        </Link>
      ) : (
        "(deleted)"
      )}
    </div>
  )
}

/**
 * A workflow's Details card beside its runs, as a scheduled query has one:
 * what starts it, the pipelines and models it runs, the charts and dashboards it draws, what
 * it reads, when it sends and where. The header above
 * already says its status, next run and when it was created, and the first
 * run in the list is its last run, so none of those repeat here.
 */
export function WorkflowDetails({ workflow: w }: { workflow: Workflow }) {
  const d = w.definition
  const quiet = d.quiet_minutes
  const conds = conditionStep(d)?.conditions ?? []
  const process = stepsInOrder(d).filter((s) => isProcessStep(s) || s.type === "chart" || s.type === "dashboard")
  // One condition reads as it is; several are numbered, and the channel list
  // says whose each channel is.
  const several = conds.length > 1
  const caseOf = (sendId: string) => d.steps.find((s) => s.id === sendId)?.case
  return (
    <Card className="p-4" data-testid="workflow-details-card">
      {/* Sticky, so a long run list does not scroll the details away. */}
      <div className="@3xl:sticky @3xl:top-20">
        <h2 className="mb-2 text-sm font-semibold text-zinc-900 dark:text-white">Details</h2>
        <dl className="divide-y divide-zinc-100 dark:divide-zinc-800">
          <DetailRow label="Trigger">{w.trigger_description}</DetailRow>
          {w.definition.trigger.type === "after_pipeline" && (
            <DetailRow label="Pipeline">
              {w.pipeline ? (
                <Link href={`/pipelines/${w.pipeline.id}`} className={link}>
                  {w.pipeline.name}
                </Link>
              ) : (
                "Deleted"
              )}
            </DetailRow>
          )}
          {process.length > 0 && (
            <DetailRow label="Steps">
              {process.map((s) => (
                <ProcessStepLine key={s.id} workflow={w} step={s} />
              ))}
            </DetailRow>
          )}
          {queryStep(d) && (
            <DetailRow label="Query">
              {w.connection
                ? `${w.connection.name} (${w.connection.connector_type ?? "connection"})`
                : "The connection was deleted."}
            </DetailRow>
          )}
          {several ? (
            <DetailRow label="Conditions">
              {conds.map((_, i) => (
                <div key={i} className="break-words">
                  {/* Otherwise is no condition: the server describes it as "otherwise send". */}
                  {isOtherwiseIn(d, i)
                    ? "Otherwise: send when no condition above is true"
                    : `Condition ${i + 1}: ${w.branch_descriptions?.[i] ?? ""}`}
                </div>
              ))}
            </DetailRow>
          ) : conds.length === 1 ? (
            <DetailRow label="Condition">{w.condition_description}</DetailRow>
          ) : null}
          {conds.length > 0 && (
            <DetailRow label="Quiet period">
              {quiet ? `${minutesWords(quiet)} after each send` : "Sends every time it is true"}
            </DetailRow>
          )}
          <DetailRow label="Sends to">
            {w.channels.length === 0
              ? "No channels"
              : w.channels.map((c) => {
                  const k = caseOf(c.node_id)
                  return (
                    <div key={`${c.node_id}:${c.id}`} className="break-words">
                      {`${several && k !== undefined ? `${conditionLabel(d, k)}: ` : ""}${c.name} (${c.kind === "slack" ? "Slack" : c.kind})`}
                    </div>
                  )
                })}
          </DetailRow>
          <DetailRow label="Edited">{formatAbsoluteTime(w.updated_at)}</DetailRow>
        </dl>
      </div>
    </Card>
  )
}
