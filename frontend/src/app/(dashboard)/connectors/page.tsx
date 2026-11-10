"use client"

import { useState, useEffect, useRef } from "react"
import { isLeakedLocalhostUrl } from "@/lib/config/api"
import { motion, MotionConfig } from "framer-motion"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import {
  Database,
  Cloud,
  Puzzle,
  Search,
  Plus,
  Zap,
  ArrowUpRight,
  ArrowDownRight,
  FileJson,
  Activity,
  Sparkles,
  ExternalLink,
  type LucideIcon,
} from "lucide-react"
import { MCPConnector, getConnectorStatusBadge, getConnectorConfidenceBadge, getConnectorConfidenceReasons } from "@/lib/types/mcp-connector"

// Category type matching the connector type definition
type ConnectorCategory = "database" | "storage" | "api" | "analytics" | "messaging" | "streaming" | "file" | "other"
import { fetchMCPConnectors, saveConnection } from "@/lib/api/mcp-connectors"
import { UnifiedConnectorModal } from "@/components/connectors/UnifiedConnectorModal"
import { ConnectionLogo } from "@/components/connectors/ConnectionLogo"
import { toast } from "sonner"
import { useRouter } from "next/navigation"
import { classifyError, type AppError } from "@/lib/utils/error-handling"
import { ErrorBanner } from "@/components/ui/error-banner"
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip"
import { canGenerateConnectors } from "@/contexts/CurrentUserContext"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { notifyTableCheck } from "@/components/connectors/ConnectionTablesCheck"
import { staggerDelay } from "@/components/kit/motion"
import { FilterChips } from "@/components/kit/FilterChips"
import { StatusPill, type PillTone } from "@/components/kit/StatusPill"
import { ListCardsSkeleton } from "@/components/kit/PageSkeleton"
import { EmptyState } from "@/components/kit/EmptyState"

// Category icons mapping
const categoryIcons: Record<string, LucideIcon> = {
  database: Database,
  storage: Cloud,
  api: Zap,
  streaming: Activity,
  file: FileJson,
  other: Puzzle,
}

// Where a connector's container is. "ready" has an image and no container yet:
// the first connection that uses it starts one (docker_status auto_deploys_on_use).
type RunState = "running" | "restarting" | "stopped" | "ready" | "local"

const RUN_STATES: RunState[] = ["running", "restarting", "stopped", "ready", "local"]

const RUN_STATE: Record<RunState, { tone: PillTone; label: string; title: string }> = {
  running: { tone: "ok", label: "Running", title: "Its container is up" },
  restarting: { tone: "wait", label: "Restarting", title: "Its container is restarting" },
  stopped: { tone: "fail", label: "Stopped", title: "Its container is deployed and stopped" },
  ready: { tone: "skip", label: "Ready", title: "Starts the first time a connection uses it" },
  local: { tone: "skip", label: "Local", title: "No container image is built for it" },
}

function runState(c: MCPConnector): RunState {
  if (c.docker_deployed && c.docker_status === "running") return "running"
  if (c.docker_deployed && c.docker_status === "restarting") return "restarting"
  if (c.docker_deployed && c.docker_status === "stopped") return "stopped"
  return c.has_dockerfile ? "ready" : "local"
}

function RunStatePill({ connector }: { connector: MCPConnector }) {
  const { tone, label, title } = RUN_STATE[runState(connector)]
  return (
    <StatusPill tone={tone} title={title}>
      {label}
    </StatusPill>
  )
}

export default function ConnectorsPage() {
  const router = useRouter()
  // Workspace role (owner/admin) gates LLM connector generation
  // (api-gateway WorkspaceGeneratorMiddleware). The active-workspace role is the
  // source of truth, not the global user role.
  const { role: workspaceRole } = useWorkspaceRole()
  const canGenerate = canGenerateConnectors(workspaceRole)
  const [connectors, setConnectors] = useState<MCPConnector[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<AppError | null>(null)
  const [searchQuery, setSearchQuery] = useState("")
  const [selectedCategory, setSelectedCategory] = useState<"all" | ConnectorCategory>("all")
  const [statusFilter, setStatusFilter] = useState<"all" | RunState>("all")
  // Cards fade in on the first load only. Once the user narrows the grid, the
  // cards a filter brings back appear at once.
  const [fadeIn, setFadeIn] = useState(true)
  const [selectedConnector, setSelectedConnector] = useState<MCPConnector | null>(null)
  const [configModalOpen, setConfigModalOpen] = useState(false)

  // Fetch connectors from MCP API
    const loadConnectors = async () => {
      try {
        setLoading(true)
      setError(null)
        const data = await fetchMCPConnectors()
      
      // Filter out internal connectors (like MinIO) that are system-only.
      //
      // sample-data was excluded here too, as "the dormant bundled demo source".
      // Dormant described the DEPLOYMENT, not the connector: it is the only
      // auth_type "none" source in the catalog and the seed image has always
      // baked it, but no compose file started a container for it, so choosing it
      // failed at connection test with a DNS miss. It is now deployed on demand
      // by the first connection that needs it (docker-compose.quickstart.yml,
      // service `sample-data-mcp`, pre-starts it only under the opt-in
      // "connectors" profile), which makes it the one source a fresh install can connect to without first producing
      // a credential -- so hiding it from the browse catalog would now hide the
      // only thing a brand-new user can actually try.
      const visibleConnectors = (data.connectors || []).filter(
        (connector) => !connector.internal
      )
      
      setConnectors(visibleConnectors)
      
      if (process.env.NODE_ENV === 'development' && visibleConnectors.length > 0) {
        console.log(`✅ Loaded ${visibleConnectors.length} connectors`)
      }
      } catch (err) {
        const e = classifyError(err, "connectors.load")
        setError(e)
        setConnectors([])
        toast.error(e.title, { description: e.hint ?? e.message })
      } finally {
        setLoading(false)
      }
    }

  useEffect(() => {
    loadConnectors()
  }, [])

  // Refetch connectors when the config modal transitions from open -> closed,
  // so a failed/cancelled save doesn't leave a stale or empty list.
  const prevConfigModalOpen = useRef(configModalOpen)
  useEffect(() => {
    if (prevConfigModalOpen.current && !configModalOpen) {
      loadConnectors()
    }
    prevConfigModalOpen.current = configModalOpen
  }, [configModalOpen])

  // Shown as written: CSS `capitalize` turned "api" into "Api" (#50).
  const categoryLabels: Record<ConnectorCategory, string> = {
    database: "Database",
    storage: "Storage",
    api: "API",
    analytics: "Analytics",
    messaging: "Messaging",
    streaming: "Streaming",
    file: "File",
    other: "Other",
  }

  // Define category display order: Database → Streaming → Storage → File → API → Other
  const categoryOrder: ConnectorCategory[] = ["database", "streaming", "storage", "file", "api", "other"]

  // Map API categories to UI categories
  const mapCategory = (apiCategory: string): ConnectorCategory => {
    const categoryMap: Record<string, ConnectorCategory> = {
      // Database types - all DB variants map to "database"
      relational_db: "database",
      nosql_db: "database",
      database: "database",
      document_db: "database",
      wide_column_db: "database",  // Cassandra, ScyllaDB, HBase
      time_series_db: "database",  // InfluxDB, TimescaleDB
      graph_db: "database",        // Neo4j, ArangoDB
      key_value_db: "database",    // Redis, DynamoDB
      search_db: "database",       // Elasticsearch, Solr
      vector_db: "database",       // Pinecone, Milvus
      data_warehouse: "database",  // Snowflake, BigQuery, Redshift (treat as DB in UI)
      // API types
      api_saas: "api",
      api: "api",
      saas: "api",
      crm: "api",
      erp: "api",
      // Streaming types
      streaming: "streaming",
      messaging: "streaming",
      event_streaming: "streaming",
      cdc: "streaming",            // Debezium, etc.
      // Storage types
      storage: "storage",
      cloud_storage: "storage",
      object_storage: "storage",
      data_lake: "storage",
      // File types
      file: "file",
      file_system: "file",
      // Utility/Other
      utility: "other",
      other: "other",
    }
    return categoryMap[apiCategory?.toLowerCase()] || "other"
  }

  // Get unique mapped categories in the defined order
  const allMappedCategories = [...new Set(connectors.map((c) => mapCategory(c.category)))]
  const categories = categoryOrder.filter(cat => allMappedCategories.includes(cat))

  // Text search
  const searched = connectors.filter(
    (c) =>
      c.display_name.toLowerCase().includes(searchQuery.toLowerCase()) ||
      c.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
      c.description?.toLowerCase().includes(searchQuery.toLowerCase())
  )
  const inCategory = (c: MCPConnector) => selectedCategory === "all" || mapCategory(c.category) === selectedCategory
  const inStatus = (c: MCPConnector) => statusFilter === "all" || runState(c) === statusFilter
  const filteredConnectors = searched.filter((c) => inCategory(c) && inStatus(c))

  // The grid shows the catalog once it has loaded; a refetch keeps the cards,
  // and so their counts. "All 0" beside the loading blocks or an error reads as
  // an empty catalog.
  const listed = !error && !(loading && connectors.length === 0)
  const counted = (n: number) => (listed ? n : undefined)
  // Each chip counts what picking it would show: the search and the other
  // filter applied, its own filter swapped for the chip.
  const categoryChips = [
    { value: "all" as const, label: "All", count: counted(searched.filter(inStatus).length) },
    ...categories.map((category) => ({
      value: category,
      label: categoryLabels[category],
      icon: categoryIcons[category] ?? Puzzle,
      count: counted(searched.filter((c) => inStatus(c) && mapCategory(c.category) === category).length),
    })),
  ]
  // Only the states some connector is in, so the toggle never offers an empty
  // "Stopped"; the picked one stays even after it empties.
  const statusChips = [
    { value: "all" as const, label: "All", count: counted(searched.filter(inCategory).length) },
    ...RUN_STATES.map((state) => ({
      value: state,
      label: RUN_STATE[state].label,
      count: counted(searched.filter((c) => inCategory(c) && runState(c) === state).length),
      present: connectors.some((c) => runState(c) === state),
    }))
      .filter((chip) => chip.present || chip.value === statusFilter)
      .map(({ present: _present, ...chip }) => chip),
  ]
  const filtered = searchQuery !== "" || selectedCategory !== "all" || statusFilter !== "all"
  const clearFilters = () => {
    setFadeIn(false)
    setSearchQuery("")
    setSelectedCategory("all")
    setStatusFilter("all")
  }

  // Group by mapped category
  const groupedConnectors = filteredConnectors.reduce<Record<string, MCPConnector[]>>(
    (acc, connector) => {
      const category = mapCategory(connector.category || "other")
      if (!acc[category]) {
        acc[category] = []
      }
      acc[category].push(connector)
      return acc
    },
    {}
  )

  // Sort grouped connectors by category order, and sort connectors within each
  // category alphabetically by display name. The backend returns connectors in
  // Go map-iteration order (randomized per request), so without this the grid
  // shuffles on every refresh.
  const sortedGroupedConnectors = Object.fromEntries(
    categoryOrder
      .filter(cat => groupedConnectors[cat])
      .map(cat => [
        cat,
        [...groupedConnectors[cat]].sort((a, b) =>
          (a.display_name || a.name).localeCompare(
            b.display_name || b.name,
            undefined,
            { sensitivity: "base" }
          )
        ),
      ])
  )

  const handleConfigure = (connector: MCPConnector) => {
    setSelectedConnector(connector)
    setConfigModalOpen(true)
  }

  const handleSaveConnection = async (config: Record<string, unknown>) => {
    try {
      // Save connection to backend
      const saved = await saveConnection(config)
      
      toast.success("Connection saved successfully", {
        description: `Connection "${(config.name as string) || "Unnamed"}" has been created`,
      })
      notifyTableCheck(saved)
      
    setConfigModalOpen(false)
    setSelectedConnector(null)
      
      // Redirect to connections page
      router.push("/connections")
    } catch (err) {
      const e = classifyError(err, "connector.save")
      toast.error(e.title, { description: e.hint ?? e.message })
      throw err // Re-throw to let the form handle it
    }
  }

  return (
    // Cards fade in; someone who asked their system for less motion gets none.
    <MotionConfig reducedMotion="user">
    <div className="space-y-5">
      <PageHeader
        heading="Data Connectors"
        description="Manage your active MCP connectors and generate new ones"
      >
        <div className="flex gap-2">
          <TooltipProvider>
            <Tooltip>
              <TooltipTrigger asChild>
                {/* span wrapper so the tooltip still fires when the button is disabled */}
                <span className={!canGenerate ? "cursor-not-allowed" : undefined}>
                  <Button
                    onClick={() => router.push("/connectors/generate")}
                    disabled={!canGenerate}
                    className="bg-gradient-to-r from-violet-600 to-indigo-600"
                  >
                    <Sparkles className="h-4 w-4 mr-2" />
                    Generate New
                  </Button>
                </span>
              </TooltipTrigger>
              {!canGenerate && (
                <TooltipContent>
                  Connector generation requires the Owner or Admin role in this workspace. Ask your workspace owner to generate it or to raise your role.
                </TooltipContent>
              )}
            </Tooltip>
          </TooltipProvider>
        </div>
      </PageHeader>

      <div className="space-y-5">
        {/* One row: what kind of connector, where its container is, and a search. */}
        <div className="flex flex-col gap-3 @3xl:flex-row @3xl:items-center @3xl:justify-between">
          <FilterChips
            label="Category"
            chips={categoryChips}
            value={selectedCategory}
            onChange={(next) => {
              setFadeIn(false)
              setSelectedCategory(next)
            }}
          />
          <div className="flex flex-col gap-3 @xl:flex-row @xl:items-center @3xl:shrink-0">
            <div className="flex items-center gap-2">
              <span aria-hidden className="text-xs font-medium text-zinc-500 dark:text-zinc-400">
                Status
              </span>
              <FilterChips
                label="Status"
                chips={statusChips}
                value={statusFilter}
                onChange={(next) => {
                  setFadeIn(false)
                  setStatusFilter(next)
                }}
              />
            </div>
            <div className="relative w-full @xl:w-64">
              <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-zinc-400" />
              <Input
                aria-label="Search connectors"
                placeholder="Search connectors..."
                value={searchQuery}
                onChange={(e) => {
                  setFadeIn(false)
                  setSearchQuery(e.target.value)
                }}
                className="pl-10"
              />
            </div>
          </div>
        </div>

        {/* First load. A refetch keeps the cards on screen. */}
        {loading && connectors.length === 0 && <ListCardsSkeleton count={6} label="Loading connectors…" />}

        {/* Error state */}
        {error && !loading && (
          <ErrorBanner error={error} onRetry={loadConnectors} />
        )}

        {/* Connectors grid */}
        {listed && (
          filteredConnectors.length === 0 ? (
            <EmptyState
              icon={filtered ? Search : Database}
              title="No connectors found"
              description={filtered ? "Nothing matches this category, status and search." : "The connector catalog is empty."}
              action={
                filtered ? (
                  <Button variant="outline" size="sm" onClick={clearFilters}>
                    Clear filters
                  </Button>
                ) : undefined
              }
            />
          ) : (
              <div className="space-y-8">
                {Object.entries(sortedGroupedConnectors).map(([category, items]) => {
                  const Icon = categoryIcons[category] || Puzzle
                  const label = categoryLabels[category as ConnectorCategory] ?? category
                  return (
                    <section key={category} aria-label={label}>
                      <div className="flex items-center gap-2 mb-4">
                        <Icon className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
                        <h2 className="text-lg font-semibold text-zinc-900 dark:text-white">
                          {label}
                        </h2>
                        <Badge variant="secondary">{items.length}</Badge>
                      </div>

                      {/* As many 17rem columns as the page's own width holds. */}
                      <div className="grid grid-cols-[repeat(auto-fill,minmax(min(17rem,100%),1fr))] gap-4">
                        {items.map((connector, index) => (
                          // One short fade, the first six staggered, on the first load only.
                          // The hover transition names its properties: `transition-all`
                          // also eased the opacity the fade sets.
                          <motion.div
                            key={connector.name}
                            initial={fadeIn ? { opacity: 0 } : false}
                            animate={{ opacity: 1 }}
                            transition={{ duration: 0.15, delay: staggerDelay(index, 0.03) }}
                            className="group relative p-4 rounded-xl border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 hover:border-violet-300 dark:hover:border-violet-700 hover:shadow-lg hover:shadow-violet-500/10 transition-[border-color,box-shadow] cursor-pointer"
                            onClick={() => handleConfigure(connector)}
                          >
                            {/* Connector icon, name, and status chip */}
                            <div className="flex items-start gap-3 mb-3">
                              <ConnectionLogo connector={connector} size="md" />
                              <div className="flex-1 min-w-0">
                                {/* Wraps to two lines before clipping: one-line `truncate`
                                    cut names that had room below them (#50). */}
                                <h3
                                  title={connector.display_name}
                                  className="font-semibold leading-snug text-zinc-900 dark:text-white line-clamp-2 break-words"
                                >
                                  {connector.display_name}
                                </h3>
                                {/* Canonical connector id/slug — disambiguates same-titled
                                    connectors (e.g. stripe vs stripe-demo) and is the
                                    connector_type users need for config. */}
                                <p
                                  title={connector.name}
                                  className="text-[11px] font-mono text-zinc-500 dark:text-zinc-400 truncate leading-tight"
                                >
                                  {connector.name}
                                </p>
                                {/* Wraps instead of overflowing: on a narrow card (the assistant
                                    docked beside the page) the chips take a second line. The
                                    container chip lives here, not beside the name, so it never
                                    squeezes the name to a letter. */}
                                <div className="mt-1 flex flex-wrap items-center gap-1.5">
                                  <RunStatePill connector={connector} />
                                  <Badge variant="secondary" className="text-xs">
                                    v{connector.version}
                                  </Badge>
                                  {(() => {
                                    // Two orthogonal chips: Status (runtime-tested?)
                                    // + Confidence (static QA quality). Confidence is
                                    // hidden when unrated so it never masks Status.
                                    const statusBadge = getConnectorStatusBadge({
                                      status: connector.status,
                                      quality_tier: connector.quality_tier,
                                      lifecycle: connector.lifecycle,
                                    })
                                    const confidenceBadge = getConnectorConfidenceBadge({
                                      confidence_level: connector.confidence_level,
                                      quality_tier: connector.quality_tier,
                                      status: connector.status,
                                      lifecycle: connector.lifecycle,
                                    })
                                    const reasons = getConnectorConfidenceReasons(connector)
                                    const tip = reasons.length ? `\n${reasons.join("\n")}` : ""
                                    return (
                                      <>
                                        <Badge
                                          variant="secondary"
                                          title={`Status: ${statusBadge.label}${tip}`}
                                          className={`text-xs ${statusBadge.className}`}
                                        >
                                          {statusBadge.label}
                                        </Badge>
                                        {confidenceBadge && (
                                          <Badge
                                            variant="secondary"
                                            title={`QA confidence: ${confidenceBadge.label}${tip}`}
                                            className={`text-xs ${confidenceBadge.className}`}
                                          >
                                            {confidenceBadge.label}
                                          </Badge>
                                        )}
                                      </>
                                    )
                                  })()}
                                  {/* Host-loopback health link: only meaningful when the browser is itself
                                      on localhost (self-host / dev). On a real deployment
                                      `http://localhost:<port>` is the USER's machine, so hide the dead link.
                                      These rows are client-fetched (never SSR'd), so no mount gate is needed. */}
                                  {connector.docker_port && connector.docker_status === "running" &&
                                    !isLeakedLocalhostUrl(`http://localhost:${connector.docker_port}/health`) && (
                                    <a
                                      href={`http://localhost:${connector.docker_port}/health`}
                                      target="_blank"
                                      rel="noopener noreferrer"
                                      onClick={(e) => e.stopPropagation()}
                                      className="inline-flex items-center gap-1 text-[10px] text-violet-600 hover:text-violet-700 dark:text-violet-400"
                                    >
                                      :{connector.docker_port}
                                      <ExternalLink className="h-2.5 w-2.5" />
                                    </a>
                                  )}
                                </div>
                              </div>
                            </div>

                            {/* Description */}
                            <p className="text-sm text-zinc-500 dark:text-zinc-400 line-clamp-2 mb-3">
                              {connector.description}
                            </p>

                            {/* QA warnings (transparent, non-blocking) */}
                            {/* Intentionally hide QA/export validation messages on cards (too noisy).
                                Detailed QA metadata remains visible inside Configure modal. */}

                            {/* Capabilities */}
                            <div className="flex flex-wrap gap-1.5 mb-3">
                              {connector.supports_source && (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-emerald-50 dark:bg-emerald-900/30 text-xs text-emerald-600 dark:text-emerald-400">
                                  <ArrowUpRight className="h-3 w-3" />
                                  Source
                                </span>
                              )}
                              {connector.supports_destination && (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-blue-50 dark:bg-blue-900/30 text-xs text-blue-600 dark:text-blue-400">
                                  <ArrowDownRight className="h-3 w-3" />
                                  Destination
                                </span>
                              )}
                              {connector.supports_cdc && (
                                <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-purple-50 dark:bg-purple-900/30 text-xs text-purple-600 dark:text-purple-400">
                                  <Zap className="h-3 w-3" />
                                  CDC
                                </span>
                              )}
                            </div>

                            {/* Actions */}
                            <div className="flex items-center justify-between pt-3 border-t border-zinc-100 dark:border-zinc-800">
                              <Button
                                variant="ghost"
                                size="sm"
                                className="text-xs"
                                onClick={(e) => {
                                  e.stopPropagation()
                                  handleConfigure(connector)
                                }}
                              >
                                <Plus className="h-3 w-3 mr-1" />
                                Configure
                              </Button>
                            </div>
                          </motion.div>
                        ))}
                      </div>
                    </section>
                  )
                })}
              </div>
            )
        )}
      </div>

      {/* Configuration Modal */}
      <UnifiedConnectorModal
        open={configModalOpen}
        onOpenChange={setConfigModalOpen}
              connector={selectedConnector}
              onSave={handleSaveConnection}
            />
    </div>
    </MotionConfig>
  )
}
