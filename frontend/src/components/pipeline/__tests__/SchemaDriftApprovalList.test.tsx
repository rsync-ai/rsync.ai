/**
 * The approval list on /pipelines/:id/schema-changes (api-gateway schema_evolution.go).
 *
 * Only a dropped source table waits for a person. Both answers take it out of the
 * pipeline; approve also drops the destination table, so it needs the checkbox, and
 * it stays decidable on a destination that is not a relational database.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
}))
const toast = vi.hoisted(() => ({ success: vi.fn(), warning: vi.fn(), info: vi.fn(), error: vi.fn() }))
vi.mock("sonner", () => ({ toast }))

import { SchemaDriftApprovalList } from "@/components/pipeline/SchemaDriftApprovalList"
import type { SchemaChange } from "@/lib/api/schema-changes"

function json(status: number, body: unknown) {
  return { ok: status < 400, status, json: async () => body }
}

function row(over: Partial<SchemaChange>): SchemaChange {
  return {
    id: "c1",
    pipeline_id: "p1",
    change_type: "drop_table",
    table_name: "public.orders",
    ddl: "DROP TABLE IF EXISTS public.orders",
    reasoning: "",
    risks: "",
    user_message: "",
    status: "pending",
    auto_applicable: true,
    reviewed_by: null,
    reviewed_at: null,
    applied_at: null,
    error_message: null,
    created_at: "2026-09-28T00:00:00Z",
    updated_at: "2026-09-28T00:00:00Z",
    ...over,
  }
}

/** GET lists `rows`; a POST answers with `post` (status + body). */
function serve(rows: SchemaChange[], post: { status: number; body: unknown } = { status: 200, body: {} }) {
  authFetch.mockImplementation(async (_url: string, init?: RequestInit) => {
    if (init?.method === "POST") return json(post.status, post.body)
    return json(200, { schema_changes: rows })
  })
}

function posted(): string[] {
  return authFetch.mock.calls
    .filter(([, init]) => (init as RequestInit | undefined)?.method === "POST")
    .map(([url]) => String(url))
}

beforeEach(() => {
  authFetch.mockReset()
  Object.values(toast).forEach((f) => f.mockReset())
})
afterEach(() => cleanup())

describe("SchemaDriftApprovalList — a dropped source table", () => {
  it("needs the checkbox before approve, and stays decidable on a non-relational destination", async () => {
    serve([row({})])
    const user = userEvent.setup()
    render(<SchemaDriftApprovalList pipelineId="p1" destConnectorType="gcs" />)

    const approve = await screen.findByRole("button", { name: "Approve — drop destination table" })
    expect(screen.getByRole("button", { name: "Reject — keep destination table" })).toBeEnabled()
    expect(screen.getByText(/Either answer removes it from the pipeline/)).toBeInTheDocument()
    expect(approve).toBeDisabled()

    await user.click(screen.getByRole("checkbox", { name: /approving deletes the destination table/ }))
    expect(approve).toBeEnabled()
  })

  it("treats a row whose statement drops a table as a dropped table, whatever its change type", async () => {
    serve([row({ change_type: "other" })])
    const user = userEvent.setup()
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    const approve = await screen.findByRole("button", { name: "Approve — drop destination table" })
    expect(approve).toBeDisabled()
    await user.click(screen.getByRole("checkbox", { name: /approving deletes the destination table/ }))
    expect(approve).toBeEnabled()
  })

  it("still blocks approving a column change on a non-relational destination", async () => {
    serve([row({ change_type: "add_column", ddl: "ALTER TABLE public.orders ADD COLUMN note TEXT" })])
    render(<SchemaDriftApprovalList pipelineId="p1" destConnectorType="gcs" />)

    expect(await screen.findByRole("button", { name: "Approve" })).toBeDisabled()
  })

  it("approve says the table left the pipeline and its destination table is being dropped", async () => {
    serve([row({})], { status: 200, body: { auto_applicable: true, drop_table: true, table_removed: true } })
    const user = userEvent.setup()
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    await user.click(await screen.findByRole("checkbox", { name: /approving deletes the destination table/ }))
    await user.click(screen.getByRole("button", { name: "Approve — drop destination table" }))

    await waitFor(() =>
      expect(toast.success).toHaveBeenCalledWith(
        "Table removed from the pipeline. Dropping its destination table now.",
      ),
    )
    expect(posted()).toEqual([expect.stringContaining("/pipelines/p1/schema-changes/c1/approve")])
  })

  it("reject says the destination table is kept", async () => {
    serve([row({})], { status: 200, body: { drop_table: true, table_removed: true } })
    const user = userEvent.setup()
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    await user.click(await screen.findByRole("button", { name: "Reject — keep destination table" }))

    await waitFor(() =>
      expect(toast.success).toHaveBeenCalledWith("Table removed from the pipeline. Its destination table is kept."),
    )
    expect(posted()).toEqual([expect.stringContaining("/pipelines/p1/schema-changes/c1/reject")])
  })

  it("shows the gateway's reason when the table cannot leave the pipeline", async () => {
    const message = "public.orders is this pipeline's only table, so it cannot be removed."
    serve([row({})], { status: 409, body: { error: "last_selected_table", message } })
    const user = userEvent.setup()
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    await user.click(await screen.findByRole("button", { name: "Reject — keep destination table" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(message))
    expect(toast.success).not.toHaveBeenCalled()
  })
})

describe("SchemaDriftApprovalList — history", () => {
  it("shows the reason rsync.ai wrote for a change it handled itself", async () => {
    const reasoning =
      "Recorded automatically. The destination keeps the column and its history; rsync.ai no longer writes values to it."
    serve([row({ change_type: "drop_column", status: "approved", reviewed_by: "system:schema-drift", reasoning })])
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    expect(await screen.findByText(reasoning)).toBeInTheDocument()
    expect(screen.queryByText(/Run it manually/)).toBeNull()
  })

  it.each([
    ["rejected", "Removed from the pipeline. The destination table and its data were kept."],
    ["approved", "Removed from the pipeline. Dropping the destination table now."],
    ["applied", "Removed from the pipeline. The destination table was dropped."],
    ["failed", "Removed from the pipeline, but the destination table was not dropped:"],
  ] as const)("says what a %s dropped table means", async (status, text) => {
    serve([row({ status, reviewed_by: "owner@example.com", error_message: status === "failed" ? "no drop_table" : null })])
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    expect(await screen.findByText(text)).toBeInTheDocument()
    if (status === "failed") expect(screen.getByText("no drop_table")).toBeInTheDocument()
  })

  // A drop question the next run disproved (healer.go WithdrawPendingTableDrops):
  // the table was only hidden because the connection's list_id was empty.
  it("shows a withdrawn drop as nothing dropped, with the setting the table needs, and asks nothing", async () => {
    const reasoning =
      "The table is still in the source, so nothing was dropped. The connection is missing the setting list_id it needs to read the table: set it on the connection and the next run reads the table."
    serve([row({ table_name: "list_task", status: "withdrawn", reviewed_by: "system:schema-drift", reasoning })])
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    expect(await screen.findByText(reasoning)).toBeInTheDocument()
    expect(screen.getByText("withdrawn")).toBeInTheDocument()
    expect(screen.getByText("No changes awaiting approval")).toBeInTheDocument()
    expect(screen.queryByText(/Removed from the pipeline/)).toBeNull()
    expect(screen.queryByRole("button", { name: /Approve/ })).toBeNull()
  })

  it("says a withdrawn drop dropped nothing even without a written reason", async () => {
    serve([row({ status: "withdrawn", reviewed_by: "system:schema-drift" })])
    render(<SchemaDriftApprovalList pipelineId="p1" />)

    expect(
      await screen.findByText(
        "Withdrawn: the table is still in the source, so nothing was dropped and it stays in this pipeline."
      )
    ).toBeInTheDocument()
  })
})
