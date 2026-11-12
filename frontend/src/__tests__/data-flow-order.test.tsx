/**
 * The Data flow tab's layout, top to bottom: CDC lag alert, the Overview tiles,
 * then Throughput and Dependencies, then the Activity feed. One card per fact:
 * - The Monitoring card (Overview / Activity sub-tabs, with its own status, run
 *   link and Pause/Resume/Stop) is gone; its two halves sit in the list.
 * - The Diagnose card is gone: the page header's Diagnose opens the same panel.
 * - The Live events card is gone: it read the same event stream as Activity.
 */

import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  useSearchParams: () => new URLSearchParams("tab=monitor"),
  usePathname: () => "/pipelines/p1",
}))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
  authFetchOrThrow: (...a: unknown[]) => authFetch(...a),
}))

vi.mock("@/components/pipeline/PipelineHealthHeader", () => ({ PipelineHealthHeader: () => null }))
vi.mock("@/components/pipeline/DataLoadingStrategyCard", () => ({ DataLoadingStrategyCard: () => null }))
vi.mock("@/components/pipeline/PipelineSchedulePanel", () => ({ PipelineSchedulePanel: () => null }))
vi.mock("@/components/pipeline/PipelineLiveStatePanel", () => ({ PipelineLiveStatePanel: () => null }))
vi.mock("@/components/pipeline/PipelineRecentRuns", () => ({ PipelineRecentRuns: () => null }))
vi.mock("@/components/pipeline/SelfHealingPanel", () => ({ SelfHealingPanel: () => null }))
vi.mock("@/components/pipeline/ExecutionHistoryTab", () => ({ ExecutionHistoryTab: () => null }))
vi.mock("@/components/pipeline/StepsDAGTab", () => ({ StepsDAGTab: () => null }))
vi.mock("@/components/pipeline/PipelineTransformsTab", () => ({ PipelineTransformsTab: () => null }))
vi.mock("@/components/pipeline/AssessmentTab", () => ({ AssessmentTab: () => null, AssessmentTabBadge: () => null }))
vi.mock("@/components/pipeline/CDCLagAlertsPanel", () => ({ CDCLagAlertsPanel: () => <div data-testid="lag-alert" /> }))
vi.mock("@/components/pipeline/PipelineMonitoringPanelNoSSR", () => ({
  PipelineMonitoringPanelNoSSR: ({ variant }: { variant: string }) => <div data-testid={`monitoring-${variant}`} />,
}))
vi.mock("@/components/pipeline/MonitoringOverviewTab", () => ({
  MonitoringOverviewTab: () => <div data-testid="overview-tiles" />,
}))

import { featureFlagsManager } from "@/config/features"

import { PipelineDetailTabsClient } from "@/components/pipeline/PipelineDetailTabsClient"
import { MonitorTab } from "@/components/pipeline/MonitorTab"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

/** `a` is rendered before `b` in document order. */
function before(a: HTMLElement, b: HTMLElement) {
  return Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING)
}

afterEach(() => {
  cleanup()
  authFetch.mockReset()
  featureFlagsManager.resetFlags()
})

describe("pipeline tabs", () => {
  it("read Overview, Runs, Steps/DAG, Table statistics, Data flow, Assessment, Transforms", () => {
    authFetch.mockResolvedValue(res(200, {}))
    render(<PipelineDetailTabsClient pipelineId="p1" pipelineType="cdc" initialTab="monitor" />)
    expect(screen.getAllByRole("tab").map((t) => t.textContent?.trim())).toEqual([
      "Overview",
      "Runs",
      "Steps/DAG",
      "Table statistics",
      "Data flow",
      "Assessment",
      "Transforms",
    ])
  })
})

describe("Data flow tab order", () => {
  it("CDC: lag alert, then the Overview tiles, then the numbers, then Activity", () => {
    authFetch.mockResolvedValue(res(200, {}))
    render(<PipelineDetailTabsClient pipelineId="p1" pipelineType="cdc" initialTab="monitor" />)

    const lag = screen.getByTestId("lag-alert")
    const overview = screen.getByTestId("overview-tiles")
    // CDC's numbers are the Flow card from the first paint: the batch
    // Throughput card no longer stands in while /runtime loads (#1572).
    const flow = screen.getByTestId("flow-card")
    const activity = screen.getByTestId("monitoring-activity")
    expect(screen.queryByText("Throughput")).toBeNull()
    expect(before(lag, overview)).toBe(true)
    expect(before(overview, flow)).toBe(true)
    expect(before(flow, activity)).toBe(true)
    // One Activity card, and no second monitoring card of any variant.
    expect(screen.queryAllByTestId(/^monitoring-/)).toHaveLength(1)
  })

  it("batch: no lag alert, the Overview tiles still first", () => {
    authFetch.mockResolvedValue(res(200, {}))
    render(<PipelineDetailTabsClient pipelineId="p1" pipelineType="etl" initialTab="monitor" />)

    expect(screen.queryByTestId("lag-alert")).not.toBeInTheDocument()
    expect(before(screen.getByTestId("overview-tiles"), screen.getByText("Throughput"))).toBe(true)
    expect(before(screen.getByText("Throughput"), screen.getByTestId("monitoring-activity"))).toBe(true)
  })

  it("control: with the monitoringOverview flag off there are no tiles, and Activity stays", () => {
    featureFlagsManager.updateFlags({ monitoringOverview: false })
    authFetch.mockResolvedValue(res(200, {}))
    render(<PipelineDetailTabsClient pipelineId="p1" pipelineType="etl" initialTab="monitor" />)

    expect(screen.queryByTestId("overview-tiles")).not.toBeInTheDocument()
    expect(screen.getByTestId("monitoring-activity")).toBeInTheDocument()
  })

  it("the numbers read Throughput, then Dependencies: no Diagnose card, no Live events card", () => {
    authFetch.mockResolvedValue(res(200, {}))
    render(<MonitorTab pipelineId="p1" />)

    // Card titles, in document order (a title can carry a badge after its name).
    const titles = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent?.trim() ?? "")
    expect(titles).toHaveLength(2)
    expect(titles[0]).toMatch(/^Throughput/)
    expect(titles[1]).toMatch(/^Dependencies/)
    expect(screen.queryByRole("button", { name: /diagnose/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/live events/i)).not.toBeInTheDocument()
  })
})
