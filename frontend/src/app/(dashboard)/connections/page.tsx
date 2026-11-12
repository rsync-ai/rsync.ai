"use client"

import { useState, useEffect } from "react"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import Link from "next/link"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Database, Plus, Search, Edit, Trash2, RefreshCw, MoreHorizontal, PlayCircle } from "lucide-react"
import { OAuthConnectButton } from "@/components/oauth/OAuthConnectButton"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { toast } from "sonner"
import {
  connectorDeployingErrorMessage,
  connectorDeployingMessage,
  isConnectorDeployingResponse,
} from "@/lib/errors/connector-deploying"
import { ConnectionLogo } from "@/components/connectors/ConnectionLogo"
import { ConnectionTestBadge, LastTestErrorLine, connectionTestState } from "@/components/connectors/ConnectionTestStatus"
import { EmptyState } from "@/components/kit/EmptyState"
import { FilterChips } from "@/components/kit/FilterChips"
import { RowsSkeleton } from "@/components/kit/PageSkeleton"
import { StatusPill } from "@/components/kit/StatusPill"
import { MAX_PIPELINE_PAGES, PIPELINE_PAGE_SIZE, listAllPipelines, pipelinesByConnection } from "@/lib/pipeline/listAllPipelines"
import { API_ENDPOINTS } from "@/lib/config/api"
import { getConnectorDisplayName } from "@/lib/types/mcp-connector"
import { httpErrorFromResponse, parseApiError, classifyError, type AppError } from "@/lib/utils/error-handling"
import type { ApiErrorBody } from "@/lib/api/types"
import { authFetch } from "@/lib/api/auth-fetch"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import { ErrorBanner } from "@/components/ui/error-banner"

interface Connection {
  id: string
  name: string
  description: string
  type: "source" | "destination"
  connector_type: string
  sync_mode?: "batch" | "cdc"
  cdc_mode?: "initial" | "streaming_only"
  // Derived by the gateway from the stored last_test_status (connections.go).
  is_connected: boolean
  last_tested_at?: string
  last_test_status?: string
  last_test_error?: string
  config?: Record<string, unknown>
  created_at: string
  updated_at: string
  // OAuth token expiry surfaced by api-gateway (BUG-2/10). is_expired is true
  // only when the linked oauth_tokens row has a real past expiry; offline
  // tokens (Shopify) report false.
  token_expires_at?: string | null
  is_expired?: boolean
}

interface OAuthToken {
  id: string
  provider: string
  expired: boolean
  expires_at: string
  account_name?: string
}

type BlockingPipeline = {
  id: string
  name: string
  status: string
  role?: "source" | "destination" | "unknown"
}

type RoleFilter = "all" | "source" | "destination"

/** How a source reads; a destination has no mode of its own. */
function modeLabel(connection: Connection): string {
  if (connection.type !== "source" || !connection.sync_mode) return "—"
  if (connection.sync_mode === "batch") return "Batch"
  return connection.cdc_mode === "streaming_only" ? "CDC, new changes only" : "CDC"
}

const PIPELINE_CAP = (MAX_PIPELINE_PAGES * PIPELINE_PAGE_SIZE).toLocaleString("en-US")

/** "3 pipelines"; with a "+" when the pipeline list stopped at its cap, so the count is a floor. */
function usedByText(count: number, truncated: boolean): string {
  if (count === 0 && !truncated) return "No pipelines"
  return `${count.toLocaleString("en-US")}${truncated ? "+" : ""} ${count === 1 && !truncated ? "pipeline" : "pipelines"}`
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`

export default function ConnectionsPage() {
  // Role gate: creating/testing connections is member+ (mirrors the backend
  // requireWorkspaceRole on CreateConnection/TestConnection). Viewers see the
  // list read-only — the create CTAs are hidden rather than failing on click.
  const { can } = useWorkspaceRole()
  const canCreate = can("create_pipeline")
  const [connections, setConnections] = useState<Connection[]>([])
  const [oauthTokens, setOAuthTokens] = useState<OAuthToken[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<AppError | null>(null)
  const [searchQuery, setSearchQuery] = useState("")
  const [activeTab, setActiveTab] = useState<RoleFilter>("all")
  // How many pipelines use each connection; null until the pipeline list is in.
  const [usedBy, setUsedBy] = useState<{ counts: Map<string, number>; truncated: boolean } | null>(null)
  const [usedByFailed, setUsedByFailed] = useState(false)
  const [deleteDialogOpen, setDeleteDialogOpen] = useState(false)
  const [connectionToDelete, setConnectionToDelete] = useState<Connection | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [deleteBlockedBy, setDeleteBlockedBy] = useState<BlockingPipeline[] | null>(null)
  const [testingId, setTestingId] = useState<string | null>(null)
  const [reconnectingId, setReconnectingId] = useState<string | null>(null)
  const [mounted, setMounted] = useState(false)

  // Fix hydration mismatch by only rendering interactive elements after mount
  useEffect(() => {
    setMounted(true)
  }, [])

  // Build a lookup map: token_id → OAuthToken (for expired checks)
  const tokenById = new Map<string, OAuthToken>(oauthTokens.map((t) => [t.id, t]))

  // Returns the OAuth token for a connection if it has one, or null
  function getConnectionToken(connection: Connection): OAuthToken | null {
    const tokenId = connection.config?.oauth_token_id as string | undefined
    if (!tokenId) return null
    return tokenById.get(tokenId) ?? null
  }

  const fetchOAuthTokens = async () => {
    const isStale = captureWorkspace()
    try {
      const response = await authFetch(API_ENDPOINTS.OAUTH.TOKENS, { cache: "no-store" })
      if (!response.ok) return
      const data = await response.json()
      if (isStale()) return
      setOAuthTokens(data.tokens || [])
    } catch {
      // Non-critical: silently ignore token fetch failures
    }
  }

  const fetchConnections = async () => {
    // A switch mid-flight makes this response the previous workspace's list.
    const isStale = captureWorkspace()
    try {
      setLoading(true)
      const response = await authFetch(API_ENDPOINTS.CONNECTIONS.LIST, { cache: "no-store" })

      if (!response.ok) {
        const body = await response.json().catch(() => ({}))
        throw Object.assign(new Error(), { statusCode: response.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || `HTTP ${response.status}` })
      }

      const data = await response.json()
      if (isStale()) return
      setConnections(data.connections || [])
      setError(null)
    } catch (err) {
      if (isStale()) return
      setError(classifyError(err, "connections.load"))
      setConnections([])
    } finally {
      setLoading(false)
    }
  }

  const fetchUsedBy = async () => {
    const isStale = captureWorkspace()
    // Until this read answers, the column shows a dash rather than counts from
    // the last read, which after a workspace switch were another workspace's.
    setUsedBy(null)
    setUsedByFailed(false)
    try {
      const { pipelines, truncated } = await listAllPipelines()
      if (isStale()) return
      const counts = new Map([...pipelinesByConnection(pipelines)].map(([id, list]) => [id, list.length]))
      setUsedBy({ counts, truncated })
      setUsedByFailed(false)
    } catch {
      if (isStale()) return
      // The column says it could not count; the list itself still shows.
      setUsedBy(null)
      setUsedByFailed(true)
    }
  }

  const handleReconnectSuccess = async (connectionId: string, newTokenId: string) => {
    try {
      setReconnectingId(connectionId)
      const connection = connections.find((c) => c.id === connectionId)
      if (!connection) return

      // Update the connection with the new token id
      const response = await authFetch(API_ENDPOINTS.CONNECTIONS.UPDATE(connectionId), {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: connection.name,
          config: { ...connection.config, oauth_token_id: newTokenId },
        }),
      })

      if (response.ok) {
        toast.success("OAuth reconnected", {
          description: "Your credentials have been refreshed successfully.",
        })
        // Refresh both connections and tokens
        await Promise.all([fetchConnections(), fetchOAuthTokens()])
      } else {
        toast.error("Reconnect failed", {
          description: "Could not update the connection with the new token.",
        })
      }
    } catch (err) {
      const e = classifyError(err, "connections.update")
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      setReconnectingId(null)
    }
  }

  useEffect(() => {
    fetchConnections()
    fetchOAuthTokens()
    fetchUsedBy()
  }, [])

  // Re-fetch when the active workspace changes (header switcher / other tab) so
  // the list never shows the previous workspace's connections. authFetch already
  // scopes each request to the active workspace; this just re-triggers it. (R4)
  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        fetchConnections()
        fetchOAuthTokens()
        fetchUsedBy()
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  )

  const handleDelete = async () => {
    if (!connectionToDelete) return
    
    try {
      setDeleting(true)
      setDeleteBlockedBy(null)
      const response = await authFetch(
        API_ENDPOINTS.CONNECTIONS.DELETE(connectionToDelete.id),
        {
          method: "DELETE",
        }
      )
      
      if (!response.ok) {
        throw await httpErrorFromResponse(response, "Failed to delete connection")
      }
      
      toast.success("Connection deleted successfully")
      await fetchConnections()
      setDeleteDialogOpen(false)
      setConnectionToDelete(null)
    } catch (err) {
      const parsed = parseApiError(err)
      const raw = parsed.raw as Record<string, unknown> | undefined
      if (parsed.statusCode === 409 && raw && (raw["pipeline_count"] || raw["pipelines"])) {
        const pipelines = Array.isArray(raw["pipelines"]) ? (raw["pipelines"] as BlockingPipeline[]) : []
        setDeleteBlockedBy(pipelines)
        toast.error("Can’t delete this connection", {
          description: `This connection is still used by ${raw["pipeline_count"] || pipelines.length} pipeline(s).`,
        })
        // Keep dialog open so the user can see what to fix.
        setDeleteDialogOpen(true)
        return
      }
      const e = classifyError(err, "connections.delete")
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      setDeleting(false)
    }
  }

  // Re-read one connection after a test, so its badge and error line show the
  // stored result (the test persists it) without reloading the whole list.
  const refreshConnection = async (connectionId: string) => {
    const isStale = captureWorkspace()
    try {
      const response = await authFetch(API_ENDPOINTS.CONNECTIONS.GET(connectionId), { cache: "no-store" })
      if (!response.ok) return
      const fresh = (await response.json()) as Connection
      if (isStale()) return
      setConnections((prev) => prev.map((c) => (c.id === connectionId ? { ...c, ...fresh } : c)))
    } catch {
      // Non-critical: the toast already reported the result.
    }
  }

  const handleTestConnection = async (connection: Connection) => {
    try {
      setTestingId(connection.id)
      const response = await authFetch(
        `${API_ENDPOINTS.CONNECTIONS.TEST_BY_ID(connection.id)}?allow_draft=true`,
        {
          method: "POST",
        }
      )
      
      const result = await response.json()
      
      if (result.success) {
        toast.success("Connection test successful", {
          description: result.message || "Connection is working properly",
        })
        await refreshConnection(connection.id)
      } else if (isConnectorDeployingResponse(result)) {
        // Connector container is still being set up on first use — not a failure.
        toast.info("Connector is still being set up", {
          description:
            connectorDeployingErrorMessage(result.error || result.message) ?? connectorDeployingMessage(),
        })
      } else {
        toast.error("Connection test failed", {
          description: result.error || result.message || "Unable to connect",
        })
        // The failure is stored too; show it under the connection.
        await refreshConnection(connection.id)
      }
    } catch (err) {
      const e = classifyError(err, "connections.test")
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      setTestingId(null)
    }
  }

  const filteredConnections = connections.filter((conn) => {
    const searchLower = searchQuery.toLowerCase()
    const matchesSearch = (conn.name?.toLowerCase() || "").includes(searchLower) ||
      (conn.connector_type?.toLowerCase() || "").includes(searchLower)
    const matchesTab = activeTab === "all" || conn.type === activeTab
    return matchesSearch && matchesTab
  })

  const sourceCount = connections.filter(c => c.type === "source").length
  const destinationCount = connections.filter(c => c.type === "destination").length
  // The chips count only a list the table shows: "All 0" beside grey rows or
  // an error reads as an empty workspace.
  const listed = !loading && !error

  // The Reconnect button's own test: an expired OAuth token that no later test has passed.
  const reconnectToken = (connection: Connection) => {
    const token = getConnectionToken(connection)
    return token?.expired && !connection.is_connected ? token : null
  }
  const failingCount = connections.filter((c) => connectionTestState(c) === "failed").length
  const reconnectCount = connections.filter(
    (c) => connectionTestState(c) === "expired" || reconnectToken(c) !== null,
  ).length

  return (
    <div className="space-y-5">
      <PageHeader
        heading="Connections"
        description="Manage your data source and destination connections"
      >
        {canCreate && (
          <Link href="/connectors">
            <Button>
              <Plus className="h-4 w-4 mr-2" />
              Add connection
            </Button>
          </Link>
        )}
      </PageHeader>

      {/* One row: the role chips with their counts, a pill for each kind of
          problem, so a failing test shows without scrolling, and the search. */}
      <div className="flex flex-col gap-3 @md:flex-row @md:items-center @md:justify-between">
        <div className="flex flex-wrap items-center gap-3">
          <FilterChips
            label="Connection role"
            value={activeTab}
            onChange={setActiveTab}
            chips={[
              { value: "all", label: "All", count: listed ? connections.length : undefined },
              { value: "source", label: "Sources", count: listed ? sourceCount : undefined },
              { value: "destination", label: "Destinations", count: listed ? destinationCount : undefined },
            ]}
          />
          {!loading && failingCount > 0 && (
            <StatusPill tone="fail">{plural(failingCount, "test failing", "tests failing")}</StatusPill>
          )}
          {!loading && reconnectCount > 0 && (
            <StatusPill tone="err">{plural(reconnectCount, "needs reconnect", "need reconnect")}</StatusPill>
          )}
        </div>

        <div className="relative w-full @md:w-72">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-zinc-400" aria-hidden />
          <Input
            placeholder="Search connections"
            aria-label="Search connections"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="pl-10"
          />
        </div>
      </div>

      {loading && (
        <Card className="px-4 py-2">
          <RowsSkeleton rows={5} label="Loading connections…" />
        </Card>
      )}

      {error && !loading && (
        <Card className="p-6">
          <ErrorBanner error={error} onRetry={fetchConnections} />
        </Card>
      )}

      {!loading && !error && connections.length === 0 && (
        <Card>
          <EmptyState
            icon={Database}
            title="No connections yet"
            description="A connection is a source to read from or a destination to write to. Add one from the connectors catalog to build a pipeline."
            action={
              canCreate ? (
                <Button asChild>
                  <Link href="/connectors">
                    <Plus className="h-4 w-4 mr-2" />
                    Browse connectors
                  </Link>
                </Button>
              ) : (
                <p className="text-sm text-zinc-500 dark:text-zinc-400">You have view-only access to this workspace.</p>
              )
            }
          />
        </Card>
      )}

      {/* One row per connection (each was a tall card). The actions column
          sticks to the right edge, so a narrow window scrolls the table
          sideways rather than pushing the actions out of reach. */}
      {!loading && !error && filteredConnections.length > 0 && (
        <Card className="overflow-hidden">
          <div className="relative w-full overflow-x-auto">
            <table className="w-full caption-bottom text-sm" aria-label="Connections">
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead className="hidden @xl:table-cell">Role</TableHead>
                  <TableHead className="hidden @3xl:table-cell">Mode</TableHead>
                  <TableHead>Last test</TableHead>
                  <TableHead className="hidden @xl:table-cell">Used by</TableHead>
                  <TableHead className="sticky right-0 z-20 border-l border-zinc-200 bg-background text-right dark:border-zinc-800">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filteredConnections.map((connection) => {
                  const token = reconnectToken(connection)
                  const count = usedBy?.counts.get(connection.id) ?? 0
                  const typeName = getConnectorDisplayName(connection.connector_type)
                  return (
                    <TableRow key={connection.id} className="group hover:bg-zinc-50 dark:hover:bg-zinc-900/50">
                      <TableCell>
                        <div className="flex min-w-0 items-center gap-3">
                          <ConnectionLogo connectorType={connection.connector_type} size="sm" category="database" />
                          <div className="min-w-0">
                            <Link
                              href={`/connections/${connection.id}?type=${connection.type}`}
                              className="block max-w-xs truncate font-medium text-zinc-900 hover:underline dark:text-white"
                              title={connection.name}
                            >
                              {connection.name}
                            </Link>
                            <div
                              className="max-w-xs truncate text-xs text-zinc-500 dark:text-zinc-400"
                              title={connection.description ? `${typeName} · ${connection.description}` : undefined}
                            >
                              {typeName}
                              {connection.description && ` · ${connection.description}`}
                            </div>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell className="hidden text-zinc-600 dark:text-zinc-400 @xl:table-cell">
                        {connection.type === "source" ? "Source" : "Destination"}
                      </TableCell>
                      <TableCell className="hidden whitespace-nowrap text-zinc-600 dark:text-zinc-400 @3xl:table-cell">
                        {modeLabel(connection)}
                      </TableCell>
                      <TableCell>
                        <ConnectionTestBadge connection={connection} />
                        <div className="mt-1 max-w-xs">
                          <LastTestErrorLine connection={connection} />
                        </div>
                      </TableCell>
                      <TableCell className="hidden whitespace-nowrap text-zinc-600 dark:text-zinc-400 @xl:table-cell">
                        {usedBy ? (
                          <span title={usedBy.truncated ? `Counted from the first ${PIPELINE_CAP} pipelines` : undefined}>
                            {usedByText(count, usedBy.truncated)}
                          </span>
                        ) : (
                          <span title={usedByFailed ? "The pipeline list did not load" : undefined}>—</span>
                        )}
                      </TableCell>
                      <TableCell className="sticky right-0 z-10 border-l border-zinc-200 bg-background/95 text-right backdrop-blur supports-[backdrop-filter]:bg-background/80 group-hover:bg-zinc-50 dark:border-zinc-800 dark:group-hover:bg-zinc-900/50">
                        <div className="flex items-center justify-end gap-2">
                          {/* Reconnect for an expired OAuth token, hidden once a test passes. */}
                          {mounted && token && (
                            <OAuthConnectButton
                              provider={token.provider}
                              displayName={token.provider}
                              label="Reconnect"
                              variant="outline"
                              size="sm"
                              disabled={reconnectingId === connection.id}
                              onSuccess={(newTokenId) => handleReconnectSuccess(connection.id, newTokenId)}
                              className="border-orange-300 text-orange-700 hover:bg-orange-50 dark:border-orange-700 dark:text-orange-300 dark:hover:bg-orange-950/30"
                            />
                          )}
                          {mounted && (
                            <DropdownMenu>
                              <DropdownMenuTrigger asChild>
                                {/* Name it after the connection, not a constant: there is one of
                                    these per row, and N identical "Actions" buttons are unusable
                                    in a screen-reader element list. */}
                                <Button variant="ghost" size="icon" className="h-8 w-8" aria-label={`Actions for ${connection.name}`}>
                                  <MoreHorizontal className="h-4 w-4" />
                                </Button>
                              </DropdownMenuTrigger>
                              <DropdownMenuContent align="end">
                                <DropdownMenuItem
                                  onClick={() => handleTestConnection(connection)}
                                  disabled={testingId === connection.id}
                                >
                                  {testingId === connection.id ? (
                                    <RefreshCw className="h-4 w-4 mr-2 animate-spin" />
                                  ) : (
                                    <PlayCircle className="h-4 w-4 mr-2" />
                                  )}
                                  Test Connection
                                </DropdownMenuItem>
                                <DropdownMenuItem asChild>
                                  {/* `edit=1` asks the detail page to open its edit form;
                                      without it this was the View Details link. */}
                                  <Link href={`/connections/${connection.id}?type=${connection.type}&edit=1`}>
                                    <Edit className="h-4 w-4 mr-2" />
                                    Edit
                                  </Link>
                                </DropdownMenuItem>
                                <DropdownMenuSeparator />
                                <DropdownMenuItem
                                  className="text-red-600"
                                  onClick={() => {
                                    setDeleteBlockedBy(null)
                                    setConnectionToDelete(connection)
                                    setDeleteDialogOpen(true)
                                  }}
                                >
                                  <Trash2 className="h-4 w-4 mr-2" />
                                  Delete
                                </DropdownMenuItem>
                              </DropdownMenuContent>
                            </DropdownMenu>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </table>
          </div>
        </Card>
      )}

      {!loading && !error && connections.length > 0 && filteredConnections.length === 0 && (
        <Card>
          <EmptyState
            size="sm"
            icon={Search}
            title="No connection matches"
            description={searchQuery ? `Nothing named or typed like "${searchQuery}" in this view.` : "No connection has this role."}
          />
        </Card>
      )}

      {/* Delete confirmation dialog */}
      <AlertDialog open={deleteDialogOpen} onOpenChange={setDeleteDialogOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete Connection</AlertDialogTitle>
            <AlertDialogDescription className="space-y-3">
              <div>
                Are you sure you want to delete &quot;{connectionToDelete?.name}&quot;? This action cannot
                be undone and will also clean up any associated replication slots and publications.
              </div>

              {deleteBlockedBy && deleteBlockedBy.length > 0 && (
                <div className="rounded-md border border-red-200 bg-red-50 p-3 text-red-900 dark:border-red-900/40 dark:bg-red-900/20 dark:text-red-100">
                  <div className="font-medium">Can’t delete: used by these pipelines</div>
                  <ul className="mt-2 list-disc space-y-1 pl-5">
                    {deleteBlockedBy.map((p) => (
                      <li key={p.id}>
                        <Link href={`/pipelines/${p.id}`} className="underline underline-offset-2">
                          {p.name || p.id}
                        </Link>{" "}
                        <span className="text-xs opacity-80">({p.role || "pipeline"}, {p.status})</span>
                      </li>
                    ))}
                  </ul>
                  <div className="mt-2 text-xs opacity-90">
                    Detach this connection from those pipelines (or delete/archive them), then try again.
                  </div>
                </div>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel
              disabled={deleting}
              onClick={() => {
                setDeleteDialogOpen(false)
                setConnectionToDelete(null)
                setDeleteBlockedBy(null)
              }}
            >
              Cancel
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              disabled={deleting}
              className="bg-red-600 hover:bg-red-700"
            >
              {deleting ? (
                <>
                  <RefreshCw className="h-4 w-4 mr-2 animate-spin" />
                  Deleting...
                </>
              ) : (
                "Delete"
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
