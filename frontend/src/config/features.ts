/**
 * Feature Flags Configuration
 * Controls availability of monitoring UI tabs and features
 * 
 * These are read from environment variables at build time (NEXT_PUBLIC_*)
 * and can be overridden at runtime via API response
 */

import React from "react"

export interface FeatureFlags {
  // Monitoring feature flags
  monitoringOverview: boolean
  monitoringInfra: boolean
  monitoringTraces: boolean

  // Plan / usage panel. A BILLING surface: it reports the plan, the pipeline
  // and query limits, trial expiry and metered transfer GB. Defaults to the
  // cloud behaviour and is turned off by the API for a deployment that does
  // not enforce plan quotas -- see api-gateway/internal/config/features.go.
  usagePanel: boolean

  // The agent gateway (the MCP server at /api/agent/v1/mcp and its tokens). On
  // in every edition unless an install sets RSYNC_AGENT_GATEWAY_ENABLED=false
  // (api-gateway/internal/config/features.go); shown only once /features says so.
  agentGateway: boolean

  // The in-app assistant ("Ask rsync.ai" in the header). On in every edition
  // unless an install sets RSYNC_ASSISTANT_ENABLED=false
  // (api-gateway/internal/config/features.go); shown only once /features says so.
  assistant: boolean

  // Whether this install allows Ask rsync.ai sample rows at all. Cloud does; the
  // self-host compose and Helm chart set RSYNC_LLM_SAMPLE_ROWS_ALLOWED=false, and
  // the gateway then refuses both switches. Hides those switches, nothing more.
  llmSampleRows: boolean

  // Whether this install has an LLM set up (llm-service's /health, relayed by
  // /api/v1/features). Gates what only an LLM can back, such as editing a
  // workflow. Not a rollout flag: off on an install without an LLM.
  llmConfigured: boolean

  // Whether this install has one workspace (RSYNC_SINGLE_WORKSPACE, the self-host
  // compose and Helm chart). The gateway refuses a second one; this only stops
  // the header offering it.
  singleWorkspace: boolean
}

/**
 * Default feature flags from environment variables
 * These are the build-time defaults
 */
export const DEFAULT_FEATURES: FeatureFlags = {
  // Monitoring Overview - the pipeline page's own health tiles. Defaults ON, in
  // lockstep with the gateway's FEATURE_MONITORING_OVERVIEW (config/features.go):
  // this flag hides the Overview SUB-TAB outright (PipelineMonitoringPanel renders
  // neither its trigger nor its content), so off meant the tiles that say
  // "Capture stopped" when Debezium has died were not on the page at all.
  // docker-compose.yml and docker-compose.prod.yml already set it "true";
  // docker-compose.staging.yml sets it "false" explicitly and keeps that.
  monitoringOverview: parseBool(process.env.NEXT_PUBLIC_FEATURE_MONITORING_OVERVIEW, true),

  // Monitoring Infrastructure tab - the ADMIN view of the platform's own workers.
  // Stays OFF by default: it is admin-only by construction and no pipeline surface
  // depends on it (pipeline alerts and consumers have their own unflagged,
  // workspace-scoped routes).
  monitoringInfra: parseBool(process.env.NEXT_PUBLIC_FEATURE_MONITORING_INFRA, false),
  
  // Monitoring Traces tab - defaults to OFF in production, ON in dev
  monitoringTraces: parseBool(
    process.env.NEXT_PUBLIC_FEATURE_MONITORING_TRACES,
    process.env.NODE_ENV === 'development'
  ),

  // Usage panel - defaults to ON, the cloud behaviour. NEXT_PUBLIC_* is
  // inlined at BUILD time and every deployment pulls the same prebuilt
  // frontend image, so this build-time value can never be what turns the
  // panel off on a self-host; /api/v1/features does that at runtime. The
  // variable exists for a deployment that builds its own image.
  usagePanel: parseBool(process.env.NEXT_PUBLIC_FEATURE_USAGE_PANEL, true),

  // Connect your AI app - OFF until /api/v1/features says the gateway serves it.
  // No build-time override: the card is useless without the gateway's routes.
  agentGateway: false,

  // The assistant - OFF until /api/v1/features says the gateway serves it.
  assistant: false,

  // Sample rows - the cloud behaviour, as usagePanel: /api/v1/features turns it
  // off on a self-host. Callers wait for that answer (useFlagState).
  llmSampleRows: true,

  // OFF until /api/v1/features says llm-service has an LLM set up.
  llmConfigured: false,

  // Cloud: any number of workspaces, until /api/v1/features says otherwise.
  singleWorkspace: false,
}

/**
 * Parse a boolean environment variable
 * @param value - Environment variable value
 * @param defaultValue - Default value if not set or invalid
 */
function parseBool(value: string | undefined, defaultValue: boolean): boolean {
  if (value === undefined || value === '') {
    return defaultValue
  }
  
  const normalized = value.toLowerCase()
  if (normalized === 'true' || normalized === '1' || normalized === 'yes') {
    return true
  }
  if (normalized === 'false' || normalized === '0' || normalized === 'no') {
    return false
  }
  
  console.warn(`Invalid boolean value for feature flag: ${value}, using default: ${defaultValue}`)
  return defaultValue
}

/**
 * Feature flags state management
 * Allows runtime override from API
 */
class FeatureFlagsManager {
  private flags: FeatureFlags = { ...DEFAULT_FEATURES }
  private listeners: Array<(flags: FeatureFlags) => void> = []
  // Whether the runtime answer from /api/v1/features has arrived yet (or
  // failed for good). Until it has, the build-time defaults are a guess, and
  // a surface that must not appear on the wrong deployment has to wait rather
  // than render the guess and retract it a frame later.
  private resolved = false

  /**
   * Get current feature flags
   */
  getFlags(): FeatureFlags {
    return { ...this.flags }
  }

  /**
   * Update feature flags (typically from API response)
   * @param updates - Partial feature flags to update
   */
  updateFlags(updates: Partial<FeatureFlags>): void {
    this.flags = { ...this.flags, ...updates }
    this.notifyListeners()
  }

  /**
   * Reset to default feature flags
   */
  resetFlags(): void {
    this.flags = { ...DEFAULT_FEATURES }
    this.resolved = false
    this.notifyListeners()
  }

  /**
   * Subscribe to feature flag changes
   * @param listener - Callback function
   * @returns Unsubscribe function
   */
  subscribe(listener: (flags: FeatureFlags) => void): () => void {
    this.listeners.push(listener)
    return () => {
      this.listeners = this.listeners.filter(l => l !== listener)
    }
  }

  /**
   * Record that the runtime flag fetch has finished, successfully or not.
   * A failed fetch still resolves: the build-time defaults are then the final
   * answer, and leaving it unresolved would hide a cloud surface forever.
   */
  markResolved(): void {
    if (this.resolved) return
    this.resolved = true
    this.notifyListeners()
  }

  /**
   * Whether the runtime flag fetch has finished.
   */
  isResolved(): boolean {
    return this.resolved
  }

  private notifyListeners(): void {
    this.listeners.forEach(listener => listener(this.flags))
  }

  /**
   * Check if any monitoring feature is enabled
   */
  isMonitoringEnabled(): boolean {
    return this.flags.monitoringOverview || this.flags.monitoringInfra || this.flags.monitoringTraces
  }

  /**
   * Check if a specific monitoring tab should be shown
   */
  shouldShowTab(tab: 'overview' | 'infrastructure' | 'traces'): boolean {
    switch (tab) {
      case 'overview':
        return this.flags.monitoringOverview
      case 'infrastructure':
        return this.flags.monitoringInfra
      case 'traces':
        return this.flags.monitoringTraces
      default:
        return false
    }
  }
}

// Singleton instance
export const featureFlagsManager = new FeatureFlagsManager()

/**
 * React hook for feature flags
 * Usage: const flags = useFeatureFlags()
 */
export function useFeatureFlags(): FeatureFlags {
  const [flags, setFlags] = React.useState<FeatureFlags>(featureFlagsManager.getFlags())

  React.useEffect(() => {
    // Ensure we start from the latest value (in case something updated before subscription)
    setFlags(featureFlagsManager.getFlags())
    return featureFlagsManager.subscribe((next) => setFlags({ ...next }))
  }, [])

  return flags
}

/**
 * Fetch feature flags from API and update manager
 * This should be called during app initialization
 */
export async function fetchFeatureFlags(apiUrl: string): Promise<void> {
  try {
    const response = await fetch(`${apiUrl}/api/v1/features`)
    if (response.ok) {
      const data = await response.json()
      featureFlagsManager.updateFlags({
        monitoringOverview: data.monitoring_overview ?? DEFAULT_FEATURES.monitoringOverview,
        monitoringInfra: data.monitoring_infra ?? DEFAULT_FEATURES.monitoringInfra,
        monitoringTraces: data.monitoring_traces ?? DEFAULT_FEATURES.monitoringTraces,
        usagePanel: data.usage_panel ?? DEFAULT_FEATURES.usagePanel,
        agentGateway: data.agent_gateway === true,
        assistant: data.assistant === true,
        llmConfigured: data.llm_configured === true,
        llmSampleRows: data.llm_sample_rows ?? DEFAULT_FEATURES.llmSampleRows,
        singleWorkspace: data.single_workspace === true,
      })
    }
  } catch (error) {
    console.warn('Failed to fetch feature flags from API, using defaults:', error)
  } finally {
    // Both branches above are final answers: a non-ok response and a network
    // failure both mean the build-time defaults are what this page gets.
    featureFlagsManager.markResolved()
  }
}

/**
 * Resolution state of a flag whose surface must not appear on the wrong
 * deployment: 'loading' until the runtime flags arrive, then 'on' or 'off'.
 * Callers must treat 'loading' as NOT visible, or a self-host would flash a
 * cloud surface before withdrawing it.
 */
export type FlagState = 'loading' | 'on' | 'off'

/**
 * React hook for one flag's resolution state.
 * Usage: const state = useFlagState('llmSampleRows')
 */
export function useFlagState(flag: keyof FeatureFlags): FlagState {
  const [state, setState] = React.useState<FlagState>('loading')

  React.useEffect(() => {
    const read = () => {
      if (!featureFlagsManager.isResolved()) {
        setState('loading')
        return
      }
      setState(featureFlagsManager.getFlags()[flag] ? 'on' : 'off')
    }
    read()
    return featureFlagsManager.subscribe(read)
  }, [flag])

  return state
}

/**
 * Resolution state of the usage panel. The panel reports plan limits, and a
 * deployment that enforces none would otherwise flash a plan meter full of
 * numbers that mean nothing before withdrawing it.
 */
export type UsagePanelState = FlagState

/**
 * React hook for the usage panel's visibility.
 * Usage: const state = useUsagePanelState()
 */
export function useUsagePanelState(): UsagePanelState {
  return useFlagState('usagePanel')
}
