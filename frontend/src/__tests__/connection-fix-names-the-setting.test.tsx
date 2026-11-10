/**
 * A run that stopped on a missing connection setting says which one, and
 * resumes on the same connection.
 *
 * Prod t-clickup (2026-10-05): a generated connector's list_id was blank. The
 * run parked on "Configure connections" showing "Source: Unknown" — a picker
 * for a connection the run already had, with no word of the setting — and its
 * Resume could not continue the run. The wait now lists the connection with the
 * setting (connection_fix_wait.go); these pin what the dialog does with it.
 */
import { describe, it, expect, vi, beforeEach, type Mock } from "vitest"
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"
import { readFileSync } from "node:fs"
import { join } from "node:path"

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(),
}))
vi.mock("sonner", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }))

import { authFetch } from "@/lib/api/auth-fetch"
import { useHITLState } from "@/lib/hooks/useHITLState"
import { useOptimisticAction } from "@/lib/pipeline/usePipelineState"
import type { PipelineUIState } from "@/lib/pipeline/pipelineStateReducer"
import { PipelineConnectionSelector } from "@/components/pipeline/PipelineConnectionSelector"
import { connectionEditHref, parseConnectionRequirements } from "@/lib/pipeline/connectionRequirements"

const PIPELINE = "9d0c1a52-0000-4000-8000-000000000001"
const EXEC = "exec-1"

// connection_fix_wait.go's wait details, as /state carries them.
const MISSING_LIST_ID = {
  request_type: "connection_config",
  action_needed: "connection_config",
  source_connection_id: "src-1",
  destination_connection_id: "dst-1",
  source_type: "t-clickup",
  destination_type: "mongodb",
  missing_config: [{ table: "list_task", field: "list_id", direction: "source" }],
  missing_connections: [
    { direction: "source", connection_id: "src-1", connector_type: "t-clickup", missing_fields: ["list_id"] },
  ],
}

const respond = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })

beforeEach(() => {
  ;(authFetch as Mock).mockReset()
})

describe("parseConnectionRequirements", () => {
  it("reads the setting to fix and the connection it belongs to", () => {
    const req = parseConnectionRequirements(MISSING_LIST_ID)
    expect(req.fixes).toEqual([
      { direction: "source", connectionId: "src-1", connectorType: "t-clickup", missingFields: ["list_id"] },
    ])
    expect(req.missing).toEqual(["source"])
    expect(req.source.connector_type).toBe("t-clickup")
    expect(req.sourceConnectionId).toBe("src-1")
    expect(req.destinationConnectionId).toBe("dst-1")
  })

  it("takes the connection from the wait when the entry omits it, and lists each setting once", () => {
    const req = parseConnectionRequirements({
      destination_connection_id: "dst-9",
      missing_connections: [{ direction: "destination", type: "bigquery", missing_fields: ["dataset", "dataset"] }],
    })
    expect(req.fixes).toEqual([
      { direction: "destination", connectionId: "dst-9", connectorType: "bigquery", missingFields: ["dataset"] },
    ])
  })

  it("keeps both producers' picker shapes, with no fix", () => {
    const temporal = parseConnectionRequirements({
      source_type: "mysql",
      missing_connections: [{ type: "postgresql", direction: "destination" }],
    })
    expect(temporal).toEqual({
      source: { connector_type: "mysql" },
      destination: { connector_type: "postgresql" },
      missing: ["destination"],
    })
    const legacy = parseConnectionRequirements({
      source_connector: "mongodb",
      missing_connections: [{ connector_type: "mongodb", direction: "source" }],
    })
    expect(legacy.source.connector_type).toBe("mongodb")
    // A resolved direction is never "unknown"; one the user must pick is.
    expect(legacy.destination.connector_type).toBe("")
    expect(parseConnectionRequirements({}).source.connector_type).toBe("unknown")
    // The two parsers had drifted: the chat alone read required_connections,
    // the pipeline page alone read required_connectors.
    expect(parseConnectionRequirements({ required_connections: { source: { connector_type: "s3" } } }).source.connector_type).toBe("s3")
    expect(parseConnectionRequirements({ required_connectors: { destination: "gcs" } }).destination.connector_type).toBe("gcs")
  })

  it("links to the connection's edit form", () => {
    expect(connectionEditHref({ connectionId: "src-1", direction: "source" })).toBe("/connections/src-1?type=source&edit=1")
  })
})

describe("the chat (useHITLState) opens the dialog on the setting", () => {
  it("passes the fix to the dialog", () => {
    const pipelineState = {
      hitl: {
        stage: "connection_config",
        blockingReason: { type: "connection_config", description: "x", details: MISSING_LIST_ID },
      },
      metadata: { executionId: EXEC },
    } as unknown as PipelineUIState
    const hook = renderHook(() =>
      useHITLState({
        pipelineId: PIPELINE,
        pipelineState,
        authoritativePipelineMetadata: null,
        optimisticAction: useOptimisticAction(vi.fn()),
        resolveHITL: vi.fn(),
        flashResumingState: vi.fn(),
      })
    )
    act(() => hook.result.current.handleOpenConnectionModal())
    expect(hook.result.current.connectionRequirements).toEqual(parseConnectionRequirements(MISSING_LIST_ID))
  })
})

describe("the pipeline page reads the same requirements", () => {
  it("uses the shared parser, not its own copy", () => {
    const src = readFileSync(join(__dirname, "../components/pipeline/PipelineLiveStatePanel.tsx"), "utf8")
    expect(src).toContain("parseConnectionRequirements(blockingDetails)")
    expect(src).not.toMatch(/Backend payloads vary by producer/)
  })
})

describe("the dialog on a missing setting", () => {
  function renderFix() {
    const posts: Array<Record<string, unknown>> = []
    ;(authFetch as Mock).mockImplementation(async (_url: string, init?: { method?: string; body?: string }) => {
      if (init?.method === "POST") {
        posts.push(JSON.parse(String(init.body)))
        return respond(200, { success: true, pipeline_id: PIPELINE, execution_id: EXEC, signal: "connections_configured" })
      }
      return respond(200, [])
    })
    const onClose = vi.fn()
    const onSuccess = vi.fn()
    render(
      <PipelineConnectionSelector
        isOpen
        onClose={onClose}
        onSuccess={onSuccess}
        pipelineId={PIPELINE}
        executionId={EXEC}
        requirements={parseConnectionRequirements(MISSING_LIST_ID)}
      />
    )
    return { posts, onClose, onSuccess }
  }

  it("names the setting and links to that connection's edit form", async () => {
    renderFix()
    expect(await screen.findByText("list_id")).toBeInTheDocument()
    expect(screen.queryByText(/Unknown/)).not.toBeInTheDocument()
    const edit = screen.getByRole("link", { name: /Edit connection/ })
    expect(edit).toHaveAttribute("href", "/connections/src-1?type=source&edit=1")
    expect(edit).toHaveAttribute("target", "_blank")
  })

  it("resumes the run on the same connections", async () => {
    const { posts, onSuccess, onClose } = renderFix()
    const resume = await screen.findByRole("button", { name: /Resume run/ })
    await waitFor(() => expect(resume).toBeEnabled())
    fireEvent.click(resume)
    await waitFor(() => expect(onSuccess).toHaveBeenCalled())
    expect(onClose).toHaveBeenCalled()
    expect(posts).toHaveLength(1)
    expect(posts[0]).toMatchObject({
      execution_id: EXEC,
      source_connection_id: "src-1",
      destination_connection_id: "dst-1",
    })
  })

  it("control: a pause with no setting still shows the picker", async () => {
    ;(authFetch as Mock).mockImplementation(async () =>
      respond(200, [{ id: "c1", name: "orders db", connector_type: "postgresql", type: "source", status: "active" }])
    )
    render(
      <PipelineConnectionSelector
        isOpen
        onClose={vi.fn()}
        pipelineId={PIPELINE}
        executionId={EXEC}
        requirements={parseConnectionRequirements({ source_type: "postgresql", missing_connections: [{ type: "postgresql", direction: "source" }] })}
      />
    )
    expect(await screen.findByRole("button", { name: /Start Pipeline/ })).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: /Edit connection/ })).not.toBeInTheDocument()
  })
})
