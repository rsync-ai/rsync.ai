/**
 * An app's API as the destination (reverse ETL) offers no Reload anywhere.
 *
 * A run into a REST/GraphQL destination updates the records it wrote before and
 * deletes nothing in the app; the gateway refuses a Reload into one
 * (pipelines.go apiDestinationReloadRefusal) and says so with
 * `reload_supported: false` on GET /pipelines/:id. Every control that starts a
 * Reload must follow that answer, or it is a button whose only outcome is a 422.
 *
 * Each case has its control: with the field absent (every other destination)
 * the Reload control is still there, so a test that cannot find a button is
 * not passing because the button never rendered at all.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn() }),
}))
vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: React.ReactNode }) => <a href={href}>{children}</a>,
}))
vi.mock("@/lib/hooks/usePipelineRuntime", () => ({ usePipelineRuntime: vi.fn() }))
vi.mock("@/lib/api/pipelines", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/pipelines")>()
  return {
    ...actual,
    getPipeline: vi.fn(),
    getPipelineCDCStatus: vi.fn(async () => ({ recovery_enabled: false })),
    executePipelineWithRunMode: vi.fn(),
  }
})

import { authFetch } from "@/lib/api/auth-fetch"
import { getPipeline } from "@/lib/api/pipelines"
import { usePipelineRuntime } from "@/lib/hooks/usePipelineRuntime"
import { PipelineActions } from "@/components/pipeline/PipelineActions"
import { CDCPipelineActions } from "@/components/pipeline/CDCPipelineActions"
import { ExecutionDetailClient } from "@/components/executions/ExecutionDetailClient"

const mockAuthFetch = vi.mocked(authFetch)
const mockGetPipeline = vi.mocked(getPipeline)
const mockRuntime = vi.mocked(usePipelineRuntime)

const PIPELINE_ID = "5d8e2ce4-27f4-43cc-a93b-44c582241f98"

function pipelineSays(fields: Record<string, unknown>) {
  mockGetPipeline.mockResolvedValue(fields as unknown as Awaited<ReturnType<typeof getPipeline>>)
}

describe("API destination: no Reload control", () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockAuthFetch.mockResolvedValue({ ok: true, json: async () => ({ status: "completed" }) } as unknown as Response)
  })

  it("pipeline header: Run stays Resume even when the default is Reload, and no Reload button", async () => {
    pipelineSays({ default_run_mode: "reload", reload_supported: false })
    render(<PipelineActions pipelineId={PIPELINE_ID} status="completed" />)

    // Before the answer lands the header shows Reload (true until told otherwise);
    // waiting for it to go is waiting for the answer.
    await waitFor(() => expect(mockGetPipeline).toHaveBeenCalledWith(PIPELINE_ID))
    await waitFor(() => expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument())
    expect(screen.getByRole("button", { name: "Run pipeline (default resume)" })).toBeInTheDocument()
  })

  it("pipeline header (control): any other destination keeps Reload", async () => {
    pipelineSays({ default_run_mode: "resume" })
    render(<PipelineActions pipelineId={PIPELINE_ID} status="completed" />)

    await waitFor(() => expect(mockGetPipeline).toHaveBeenCalled())
    expect(await screen.findByRole("button", { name: /reload pipeline/i })).toBeInTheDocument()
  })

  it("CDC header: no Reload in the recovery cluster", async () => {
    mockAuthFetch.mockResolvedValue({ ok: true, json: async () => ({ status: "running" }) } as unknown as Response)
    mockRuntime.mockReturnValue({ runtime: { phase: "failed" } } as unknown as ReturnType<typeof usePipelineRuntime>)
    render(<CDCPipelineActions pipelineId={PIPELINE_ID} pipelineName="p5" status="running" reloadSupported={false} />)

    await waitFor(() => expect(screen.getByRole("button", { name: /restart cdc/i })).toBeInTheDocument())
    expect(screen.queryByRole("button", { name: /reload/i })).not.toBeInTheDocument()
  })

  it("CDC header (control): the prop absent keeps Reload", async () => {
    mockAuthFetch.mockResolvedValue({ ok: true, json: async () => ({ status: "running" }) } as unknown as Response)
    mockRuntime.mockReturnValue({ runtime: { phase: "failed" } } as unknown as ReturnType<typeof usePipelineRuntime>)
    render(<CDCPipelineActions pipelineId={PIPELINE_ID} pipelineName="p5" status="running" />)

    await waitFor(() => expect(screen.getByRole("button", { name: /restart cdc/i })).toBeInTheDocument())
    expect(screen.getByRole("button", { name: /reload/i })).toBeInTheDocument()
  })

  it("execution page: Re-run (Resume) only", async () => {
    pipelineSays({ reload_supported: false })
    render(<ExecutionDetailClient execution={{ id: "exec-1", status: "success", pipelineId: PIPELINE_ID }} />)

    await waitFor(() => expect(mockGetPipeline).toHaveBeenCalledWith(PIPELINE_ID))
    await waitFor(() => expect(screen.queryByRole("button", { name: /re-run \(reload\)/i })).not.toBeInTheDocument())
    expect(screen.getByRole("button", { name: /re-run \(resume\)/i })).toBeInTheDocument()
  })

  it("execution page (control): any other destination keeps Re-run (Reload)", async () => {
    pipelineSays({})
    render(<ExecutionDetailClient execution={{ id: "exec-1", status: "success", pipelineId: PIPELINE_ID }} />)

    await waitFor(() => expect(mockGetPipeline).toHaveBeenCalledWith(PIPELINE_ID))
    expect(screen.getByRole("button", { name: /re-run \(reload\)/i })).toBeInTheDocument()
  })
})
