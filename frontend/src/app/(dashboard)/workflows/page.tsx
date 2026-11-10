"use client"

import { useEffect, useState, useSyncExternalStore } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { CirclePause, CirclePlay, PencilLine, Plus, Sparkles, XCircle } from "lucide-react"

import { formatNextRun } from "@/components/explorer/scheduleTime"
import { ASSISTANT_EXAMPLES, readHomeAskMode } from "@/components/home/HomeAskBox"
import { WorkflowNodes } from "@/components/icons/WorkflowNodes"
import { EmptyState } from "@/components/kit/EmptyState"
import { RowsSkeleton, StatCardsSkeleton } from "@/components/kit/PageSkeleton"
import { StatCard } from "@/components/kit/StatCard"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { WorkflowStatusPill } from "@/components/workflows/WorkflowReview"
import { RunOutcomeBadge, runOutcome } from "@/components/workflows/runOutcome"
import { CHANNEL_KIND_ICON, STEP_KIND_STYLE, workflowStepIcons } from "@/components/workflows/stepIcons"
import { usePollWhileRunning } from "@/components/workflows/usePollWhileRunning"
import { featureFlagsManager } from "@/config/features"
import { listWorkflows, type Workflow, type WorkflowChannel } from "@/lib/api/workflows"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { cn, formatAbsoluteTime } from "@/lib/utils"
import { onWorkflowChanged } from "@/lib/events/workflowChanged"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/**
 * The workspace's workflows: reports and data alerts that run a query on a
 * trigger and send a message to a delivery channel. They are drafted in Ask
 * rsync.ai ("every Monday 9am, email finance last week's revenue"), or built by
 * an owner or admin in the editor (/workflows/new: a query, its conditions and
 * a message for each), and turned on from the draft's card or this list.
 */
/** Each channel once: the API lists a channel once per branch that sends to it. */
function uniqueChannels(channels: Workflow["channels"]): WorkflowChannel[] {
  return [...new Map(channels.map((c) => [c.id, c])).values()]
}

const subscribeFlags = (onChange: () => void) => featureFlagsManager.subscribe(() => onChange())

export default function WorkflowsPage() {
  const router = useRouter()
  const [workflows, setWorkflows] = useState<Workflow[] | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [version, setVersion] = useState(0)

  // "New workflow" opens Ask rsync.ai with an example to start from. Without
  // the assistant (its flag off or no model) there is nothing to draft one
  // with, and the button is not shown. An owner or admin can always build one
  // in the editor; beside the assistant, that button is the secondary one.
  const canDraft = useSyncExternalStore(subscribeFlags, readHomeAskMode, () => "loading" as const) === "assistant"
  const newWorkflow =
    canDraft || canManage ? (
      <div className="flex flex-wrap items-center gap-2">
        {canDraft && (
          <Button
            size="sm"
            className="bg-violet-600 text-white hover:bg-violet-700"
            onClick={() => useAssistantStore.getState().openWith(null, ASSISTANT_EXAMPLES[2].text)}
          >
            <Sparkles className="mr-1.5 h-4 w-4" aria-hidden />
            New workflow
          </Button>
        )}
        {canManage && (
          <Button asChild size="sm" variant={canDraft ? "outline" : "default"}>
            <Link href="/workflows/new">
              <Plus className="mr-1.5 h-4 w-4" aria-hidden />
              Build it yourself
            </Link>
          </Button>
        )}
      </div>
    ) : null

  // A workspace switch (the header switcher or another tab) clears the list and
  // asks again: this is a client page, so nothing else re-reads it.
  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setWorkflows(null)
        setCanManage(false)
        setError(null)
        setVersion((v) => v + 1)
      }),
    [],
  )

  // A workflow drafted, turned on or changed from Ask rsync.ai's panel beside the
  // list: read the list again, keeping the rows shown until the answer lands.
  useEffect(() => onWorkflowChanged(() => setVersion((v) => v + 1)), [])

  // A response that lands after the page unmounted, or after a workspace switch
  // overtook it, is dropped: it holds the other workspace's workflows.
  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    listWorkflows().then(
      ({ workflows, can_manage }) => {
        if (!alive || isStale()) return
        setWorkflows(workflows)
        setCanManage(can_manage)
        setError(null)
      },
      (err: unknown) => {
        if (alive && !isStale()) setError(err instanceof Error ? err.message : "Could not load workflows")
      },
    )
    return () => {
      alive = false
    }
  }, [version])

  // While a workflow's last run is going, read the list again so its Last run
  // follows. A failed re-read keeps the list shown; the next tick tries again.
  usePollWhileRunning(workflows?.some((w) => w.last_run?.status === "running") ?? false, async () => {
    const isStale = captureWorkspace()
    const got = await listWorkflows().catch(() => null)
    if (got && !isStale()) setWorkflows(got.workflows)
  })

  const loading = workflows === null && !error

  return (
    <div className="space-y-5">
      <PageHeader
        heading="Workflows"
        description="Reports and data alerts: on a schedule or after a pipeline, run pipelines, rebuild models or run a query, check the result, and send a message, chart or dashboard to Slack or email."
      >
        {workflows !== null && workflows.length > 0 && newWorkflow}
      </PageHeader>

      {error && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {error}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
      )}
      {loading && (
        <>
          <StatCardsSkeleton count={5} label="Loading workflow counts…" className="gap-3 grid-cols-2 @3xl:grid-cols-5" />
          <div className="rounded-lg border border-zinc-200 dark:border-zinc-800">
            <RowsSkeleton rows={4} label="Loading workflows…" className="px-4" />
          </div>
        </>
      )}
      {workflows !== null && workflows.length === 0 && (
        <div className="rounded-lg border border-zinc-200 dark:border-zinc-800">
          <EmptyState
            icon={WorkflowNodes}
            title="No workflows yet"
            description={
              <>
                {canDraft && (
                  <>
                    Ask rsync.ai drafts one from a sentence, for example &ldquo;every Monday at 9:00 send #finance last
                    week&apos;s revenue&rdquo; or &ldquo;alert on-call if no orders arrive for 2 hours&rdquo;.{" "}
                  </>
                )}
                {canManage
                  ? `${canDraft ? "Or build one yourself" : "Build one yourself"}: a query, the conditions to check, and a message for each. `
                  : !canDraft && "Workspace owners and admins build workflows. "}
                A workflow sends to the workspace&apos;s{" "}
                <Link href="/workspace/settings?tab=channels" className="font-medium underline">
                  delivery channels
                </Link>
                . It never moves data; a pipeline does.
              </>
            }
            action={newWorkflow}
          />
        </div>
      )}
      {workflows !== null && workflows.length > 0 && (
        <>
          <WorkflowCounts workflows={workflows} />
          <div className="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800">
            <table className="w-full text-left text-sm">
              <thead className="bg-zinc-50 text-xs text-zinc-500 dark:bg-zinc-950 dark:text-zinc-400">
                <tr>
                  <th className="px-3 py-2 font-medium">Workflow</th>
                  <th className="px-3 py-2 font-medium">Steps</th>
                  <th className="px-3 py-2 font-medium">Sends to</th>
                  <th className="px-3 py-2 font-medium">Status</th>
                  <th className="px-3 py-2 font-medium">Last run</th>
                  <th className="px-3 py-2 font-medium">Created</th>
                </tr>
              </thead>
              <tbody>
                {workflows.map((w) => (
                  // The whole row opens the workflow; the name stays a link, so a
                  // keyboard or a middle click still has one.
                  <tr
                    key={w.id}
                    className="cursor-pointer border-t border-zinc-100 align-top hover:bg-zinc-50 dark:border-zinc-800 dark:hover:bg-zinc-900/50"
                    onClick={() => router.push(`/workflows/${w.id}`)}
                  >
                    <td className="px-3 py-2">
                      <Link
                        href={`/workflows/${w.id}`}
                        onClick={(e) => e.stopPropagation()}
                        className="font-medium text-zinc-900 hover:underline dark:text-white"
                      >
                        {w.name}
                      </Link>
                      <div className="mt-0.5 text-xs text-zinc-500 dark:text-zinc-400">
                        {w.trigger_description}
                        {w.status === "active" && w.next_run_at && (
                          <span title={formatAbsoluteTime(w.next_run_at)}> · {nextRunWords(w.next_run_at)}</span>
                        )}
                      </div>
                    </td>
                    <td className="px-3 py-2">
                      <StepIcons workflow={w} />
                    </td>
                    <td className="px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                      <ChannelList channels={uniqueChannels(w.channels)} />
                    </td>
                    <td className="px-3 py-2">
                      <WorkflowStatusPill workflow={w} />
                    </td>
                    <td className="px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                      {w.last_run ? (
                        <div className="flex flex-col items-start gap-1">
                          <RunOutcomeBadge outcome={runOutcome(w.last_run)} />
                          <LocalDateTime value={w.last_run.started_at} />
                        </div>
                      ) : (
                        <span className="text-zinc-500 dark:text-zinc-400">Never</span>
                      )}
                    </td>
                    <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                      <LocalDateTime value={w.created_at} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  )
}

/** "next in 5d"; "due now" in the moment between a tick firing and the list re-read. */
function nextRunWords(iso: string): string {
  const words = formatNextRun(iso)
  return words.startsWith("in ") ? `next ${words}` : words
}

/** The workspace's workflows by status, and how many last ran into a failure. */
function WorkflowCounts({ workflows }: { workflows: Workflow[] }) {
  const count = (status: Workflow["status"]) => workflows.filter((w) => w.status === status).length
  const failed = workflows.filter((w) => w.last_run && runOutcome(w.last_run) === "failed").length
  return (
    <div data-testid="workflow-counts" className="grid grid-cols-2 gap-3 @3xl:grid-cols-5">
      <StatCard title="Total" icon={WorkflowNodes} value={workflows.length} hint="Every status" />
      <StatCard title="On" icon={CirclePlay} value={count("active")} hint="Running on their trigger" />
      <StatCard title="Paused" icon={CirclePause} value={count("paused")} hint="Off until turned on again" />
      <StatCard title="Drafts" icon={PencilLine} value={count("draft")} hint="Off: they run only on Run now" />
      <StatCard
        title="Last run failed"
        icon={XCircle}
        value={failed}
        tone={failed > 0 ? "bad" : "default"}
        hint={failed > 0 ? "Its Runs tab has the error" : "No workflow's last run failed"}
      />
    </div>
  )
}

/** The workflow's steps as icons, in run order, each named for a screen reader and on hover. */
function StepIcons({ workflow: w }: { workflow: Workflow }) {
  const steps = workflowStepIcons(w)
  if (steps.length === 0) return <span className="text-xs text-zinc-500 dark:text-zinc-400">—</span>
  return (
    <span className="flex flex-wrap items-center gap-1">
      {steps.map(({ icon: Icon, label, kind }, i) => (
        <span
          key={i}
          title={label}
          className={cn("flex h-6 w-6 items-center justify-center rounded-md", STEP_KIND_STYLE[kind].tile)}
        >
          <Icon className={cn("h-3.5 w-3.5", STEP_KIND_STYLE[kind].icon)} aria-hidden />
        </span>
      ))}
      <span className="sr-only">{steps.map((s) => s.label).join(", then ")}</span>
    </span>
  )
}

function ChannelList({ channels }: { channels: WorkflowChannel[] }) {
  if (channels.length === 0) return <span className="text-zinc-500 dark:text-zinc-400">—</span>
  return (
    <ul className="flex flex-col gap-0.5">
      {channels.map((c) => {
        const { icon: Icon, label } = CHANNEL_KIND_ICON[c.kind]
        return (
          <li key={c.id} className="flex items-center gap-1.5">
            <Icon className="h-3.5 w-3.5 shrink-0 text-zinc-500 dark:text-zinc-400" aria-label={label} />
            <span className="truncate">{c.name}</span>
          </li>
        )
      })}
    </ul>
  )
}
