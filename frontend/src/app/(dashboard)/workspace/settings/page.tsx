"use client"

import { useSearchParams } from "next/navigation"
import { PageHeader } from "@/components/layout/PageHeader"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Badge } from "@/components/ui/badge"
import { Loader2 } from "lucide-react"
import { useWorkspace } from "@/contexts/WorkspaceContext"
import { WorkspaceGeneralSettings } from "@/components/workspace/WorkspaceGeneralSettings"
import { WorkspaceMembers } from "@/components/workspace/WorkspaceMembers"
import { WorkspaceRolesMatrix } from "@/components/workspace/WorkspaceRolesMatrix"
import { SavedQueryRetentionCard } from "@/components/workspace/SavedQueryRetentionCard"
import { WorkspaceLLMDataCard } from "@/components/workspace/WorkspaceLLMDataCard"
import { DeliveryChannelsCard } from "@/components/workflows/DeliveryChannelsCard"
import { ModelLayersCard } from "@/components/workspace/ModelLayersCard"
import { ROLE_LABELS, roleBadgeVariant, meetsRole, type WorkspaceRole } from "@/lib/workspace/roles"

const VALID_TABS = ["general", "members", "roles", "data", "layers", "channels"] as const
type SettingsTab = (typeof VALID_TABS)[number]

/**
 * Workspace settings hub for the ACTIVE workspace (chosen in the header switcher).
 * Tabs: General (rename/leave), Members & invitations, Roles & permissions. The
 * active workspace + the caller's role come from the shared WorkspaceProvider, so
 * this page never re-fetches them. /workspace/members redirects here (?tab=members).
 */
export default function WorkspaceSettingsPage() {
  const { activeWorkspace, isLoading, error, refresh } = useWorkspace()
  const searchParams = useSearchParams()

  const requested = searchParams.get("tab")
  const initialTab: SettingsTab = (VALID_TABS as readonly string[]).includes(requested ?? "")
    ? (requested as SettingsTab)
    : "general"

  if (isLoading) {
    return (
      <div className="space-y-6">
        <PageHeader heading="Workspace settings" description="Manage your workspace, members and roles." />
        <div className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading…
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className="space-y-6">
        <PageHeader heading="Workspace settings" description="Manage your workspace, members and roles." />
        <div className="flex items-center gap-3 text-sm">
          <span className="text-red-600 dark:text-red-400">Couldn&apos;t load your workspaces — try again.</span>
          <button
            className="rounded border border-zinc-300 px-2 py-1 text-xs hover:bg-zinc-50 dark:border-zinc-700 dark:hover:bg-zinc-800"
            onClick={() => void refresh()}
          >
            Retry
          </button>
        </div>
      </div>
    )
  }

  if (!activeWorkspace) {
    return (
      <div className="space-y-6">
        <PageHeader heading="Workspace settings" description="Manage your workspace, members and roles." />
        <p className="text-sm text-zinc-500 dark:text-zinc-400">
          No workspace selected. Use the workspace switcher in the header to pick one.
        </p>
      </div>
    )
  }

  const role = activeWorkspace.role
  const roleLabel = ROLE_LABELS[role as WorkspaceRole] ?? role
  const isReadOnly = !meetsRole(role, "member")

  return (
    <div className="space-y-5">
      {/* The caller's role sits beside the title, so they always know their
          access level without a banner row of its own. */}
      <PageHeader
        heading="Workspace settings"
        badge={
          <Badge variant={roleBadgeVariant(role)} title="Your role in this workspace">
            <span className="sr-only">Your role: </span>
            {roleLabel}
          </Badge>
        }
        description={
          <>
            {`Manage "${activeWorkspace.name}" — members, roles and workspace details.`}
            {isReadOnly && " Your access is read-only; ask an admin for more."}
          </>
        }
      />

      <Tabs defaultValue={initialTab} className="space-y-4">
        <TabsList aria-label="Workspace settings" className="h-auto flex-wrap justify-start">
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="members">Members &amp; invitations</TabsTrigger>
          <TabsTrigger value="roles">Roles &amp; permissions</TabsTrigger>
          <TabsTrigger value="data">Data</TabsTrigger>
          <TabsTrigger value="layers">Model layers</TabsTrigger>
          <TabsTrigger value="channels">Delivery channels</TabsTrigger>
        </TabsList>

        <TabsContent value="general">
          <WorkspaceGeneralSettings
            workspaceId={activeWorkspace.id}
            workspaceName={activeWorkspace.name}
            slug={activeWorkspace.slug}
            currentRole={role}
            isPersonal={activeWorkspace.is_personal}
          />
        </TabsContent>

        <TabsContent value="members">
          <WorkspaceMembers
            workspaceId={activeWorkspace.id}
            workspaceName={activeWorkspace.name}
            currentRole={role}
            isPersonal={activeWorkspace.is_personal}
          />
        </TabsContent>

        <TabsContent value="roles">
          <WorkspaceRolesMatrix currentRole={role} />
        </TabsContent>

        {/* Settings about what happens to the workspace's data: what Ask rsync.ai
            may send a model, and what version history is thrown away, and
            when. Kept as its own tab rather than buried in General because an
            owner should be able to answer both in one place. */}
        <TabsContent value="data" className="space-y-4">
          <WorkspaceLLMDataCard />
          <SavedQueryRetentionCard currentRole={role} />
        </TabsContent>

        {/* The workspace's own labels for its models, in their order. */}
        <TabsContent value="layers">
          <ModelLayersCard />
        </TabsContent>

        {/* Where workflows send (Slack channels, email lists). */}
        <TabsContent value="channels">
          <DeliveryChannelsCard />
        </TabsContent>
      </Tabs>
    </div>
  )
}
