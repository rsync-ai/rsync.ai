/**
 * Test helpers (not a test file: vitest runs only *.test.* here): build a
 * definition version 3 the way the server stores a query-and-conditions
 * workflow (shared/go/workflowdef FromBranches: query, condition, then
 * send_1..send_N, one per condition), and a channel of condition i's send.
 */

import type {
  WorkflowChannel,
  WorkflowCondition,
  WorkflowDefinition,
  WorkflowMessage,
} from "@/lib/api/workflows"

export interface FixtureBranch {
  condition: WorkflowCondition
  message: WorkflowMessage
}

export function branchesDefinition(opts: {
  trigger?: WorkflowDefinition["trigger"]
  query: { connection_id: string; sql: string }
  branches: FixtureBranch[]
  quiet_minutes?: number
}): WorkflowDefinition {
  const d: WorkflowDefinition = {
    version: 3,
    trigger: opts.trigger ?? { type: "manual" },
    steps: [
      { id: "query", type: "query", connection_id: opts.query.connection_id, sql: opts.query.sql },
      {
        id: "condition",
        type: "condition",
        after: ["query"],
        rule: "all_success",
        conditions: opts.branches.map((b) => b.condition),
      },
      ...opts.branches.map((b, i) => ({
        id: `send_${i + 1}`,
        type: "send" as const,
        after: ["condition"],
        rule: "all_success" as const,
        case: i,
        message: b.message,
        required: true,
      })),
    ],
  }
  if (opts.quiet_minutes !== undefined) d.quiet_minutes = opts.quiet_minutes
  return d
}

/** Each condition of d, with its send's message, top to bottom. */
export function fixtureBranches(d: WorkflowDefinition): FixtureBranch[] {
  const conds = d.steps.find((s) => s.type === "condition")?.conditions ?? []
  return conds.map((condition, i) => ({
    condition,
    message: d.steps.find((s) => s.type === "send" && s.case === i)?.message ?? { subject: "", body: "" },
  }))
}

/** A channel of condition `branch`'s send (send_<branch+1>). */
export function caseChannel(c: Omit<WorkflowChannel, "node_id"> & { branch: number }): WorkflowChannel {
  const { branch, ...rest } = c
  return { ...rest, node_id: `send_${branch + 1}` }
}
