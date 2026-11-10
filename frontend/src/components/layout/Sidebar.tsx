"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { cn } from "@/lib/utils"
import { useUIStore } from "@/lib/store/useUIStore"
import { Button } from "@/components/ui/button"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Tooltip, TooltipContent, TooltipTrigger, TooltipProvider } from "@/components/ui/tooltip"
import {
  Home,
  GitBranch,
  Database,
  Settings,
  ChevronLeft,
  ChevronRight,
  History,
  Cable,
  Boxes,
  Network,
  Search,
  Zap,
  Shield,
  Building2,
  Gauge,
  LayoutDashboard,
  ChartColumn,
  X,
} from "lucide-react"
import { RsyncLogo } from "@/components/icons/RsyncLogo"
import { WorkflowNodes } from "@/components/icons/WorkflowNodes"
import { useUsagePanelState } from "@/config/features"

// Five groups, four of them headed. It was eight one- or two-item groups, each
// with its own heading, which put Settings and Admin below the fold on an
// ordinary laptop screen (#56). "Build" holds the three things that run — a
// pipeline, a scheduled query, a workflow — and Runs lists all of their runs.
// "Analyze" is where data is read (dashboards-design.md § 7.4): Dashboards,
// Charts and the Explorer.
const navigation: Array<{
  title?: string
  items: Array<{
    name: string
    href: string
    icon: typeof Home
    exact?: boolean
    /** Other path prefixes that light this row, e.g. the pipeline builder at /chat. */
    alsoActive?: string[]
    adminOnly?: boolean
  }>
}> = [
  {
    items: [
      { name: "Home", href: "/", icon: Home },
      // A pipeline run still opens at /executions/:id, and belongs to this list.
      { name: "Runs", href: "/runs", icon: History, alsoActive: ["/executions"] },
    ],
  },
  {
    title: "Build",
    items: [
      // The pipeline builder (/chat) has no row of its own: it is where "New
      // pipeline" and Home's box (without an LLM) lead, so it lights Pipelines.
      // No "Transform Builder": a pipeline's transforms are added and edited in
      // its Transforms tab, and /transforms only redirects there.
      { name: "Pipelines", href: "/pipelines", icon: GitBranch, alsoActive: ["/chat"] },
      // A saved query that builds a table or runs a statement, scheduled or not. The
      // page was /explorer/schedules; next.config.ts redirects the old links.
      { name: "Models", href: "/models", icon: Boxes },
      // Reports and data alerts on data rsync.ai already loaded; drafted in Ask rsync.ai.
      { name: "Workflows", href: "/workflows", icon: WorkflowNodes },
    ],
  },
  {
    title: "Analyze",
    items: [
      // A dashboard Ask rsync.ai drafted opens at /dashboard-proposals/:id until it is saved.
      { name: "Dashboards", href: "/dashboards", icon: LayoutDashboard, alsoActive: ["/dashboard-proposals"] },
      { name: "Charts", href: "/charts", icon: ChartColumn },
      // `exact` because /explorer/lineage, in Data, is a CHILD in the URL: without
      // it the prefix match lights up two rows at once.
      { name: "Explorer", href: "/explorer", icon: Search, exact: true },
    ],
  },
  {
    title: "Data",
    items: [
      { name: "Lineage", href: "/explorer/lineage", icon: Network },
      // Not `adminOnly`: /pii carries no role gate of its own, so hiding the row
      // would only hide a page every user can still reach by typing the URL.
      { name: "PII Management", href: "/pii", icon: Database },
      { name: "Connections", href: "/connections", icon: Cable },
      { name: "Connectors", href: "/connectors", icon: Zap },
    ],
  },
  {
    title: "Manage",
    items: [
      { name: "Workspace", href: "/workspace/settings", icon: Building2 },
      { name: "Usage", href: "/usage", icon: Gauge },
      { name: "Settings", href: "/settings", icon: Settings },
      { name: "Admin", href: "/admin", icon: Shield, adminOnly: true },
    ],
  },
]

interface SidebarProps {
  role?: string
}

export function Sidebar({ role }: SidebarProps) {
  const pathname = usePathname()
  const collapsed = useUIStore((state) => state.sidebarCollapsed)
  const mobileOpen = useUIStore((state) => state.sidebarOpen)
  const setSidebarOpen = useUIStore((state) => state.setSidebarOpen)
  const toggleSidebarCollapsed = useUIStore((state) => state.toggleSidebarCollapsed)
  const usagePanel = useUsagePanelState()

  const underPrefix = (prefix: string) => pathname === prefix || pathname.startsWith(prefix + "/")
  const isActiveLink = (href: string, exact = false, alsoActive: string[] = []) => {
    if (href === "/") return pathname === "/" || pathname === "/dashboard"
    if (alsoActive.some(underPrefix)) return true
    if (exact) return pathname === href
    return underPrefix(href)
  }

  // NOTE: `role` here is the PLATFORM role from /auth/me (gates the operator-only
  // /admin area), NOT a workspace role. Both can be the string "admin" but they are
  // unrelated — a workspace admin is not a platform admin. Workspace-scoped gating
  // lives in useWorkspaceRole()/can(); never conflate the two.
  const isAdmin = role === "admin"

  const navContent = (isMobile = false) => (
    <ScrollArea className="flex-1 py-4">
      <nav className="space-y-5 px-2">
        {navigation.map((section) => (
          <div key={section.title ?? section.items[0].href}>
            {section.title && (!collapsed || isMobile) && (
              <h4 className="mb-2 px-2 text-xs font-semibold uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
                {section.title}
              </h4>
            )}
            <div className="space-y-1">
              {section.items.map((item) => {
                // The Usage entry opens the plan/quota panel: plan, pipeline and
                // query limits, trial expiry, metered transfer GB. A deployment
                // that does not enforce plans has none of those, so the entry is
                // withheld until the runtime flags say otherwise -- 'loading' is
                // deliberately not visible, or a self-host would flash it.
                if (item.href === "/usage" && usagePanel !== "on") return null
                if (item.adminOnly && !isAdmin) return null

                const isActive = isActiveLink(item.href, item.exact, item.alsoActive)
                const Icon = item.icon

                const linkContent = (
                  <Link
                    href={item.href}
                    prefetch={false}
                    // The collapsed rail renders the icon ALONE — no <span>, and the
                    // Radix tooltip is only a description, and only while hovered — so
                    // without this every nav link is nameless (axe `link-name`, serious)
                    // for anyone who has collapsed the rail. Set unconditionally: in the
                    // expanded branch it is identical to the visible text.
                    aria-label={item.name}
                    onClick={() => isMobile && setSidebarOpen(false)}
                    className={cn(
                      "flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-all",
                      isActive
                        ? "bg-zinc-100 text-zinc-900 dark:bg-zinc-800 dark:text-white"
                        : "text-zinc-600 hover:bg-zinc-50 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800/50 dark:hover:text-white",
                      collapsed && !isMobile && "justify-center px-2"
                    )}
                  >
                    <Icon className={cn("h-5 w-5 shrink-0", isActive && "text-violet-600")} />
                    {(!collapsed || isMobile) && <span>{item.name}</span>}
                  </Link>
                )

                if (collapsed && !isMobile) {
                  return (
                    <Tooltip key={item.name}>
                      <TooltipTrigger asChild>{linkContent}</TooltipTrigger>
                      <TooltipContent side="right">{item.name}</TooltipContent>
                    </Tooltip>
                  )
                }

                return <div key={item.name}>{linkContent}</div>
              })}
            </div>
          </div>
        ))}
      </nav>
    </ScrollArea>
  )

  return (
    <TooltipProvider delayDuration={0}>
      {/* ── Desktop sidebar (md+) ── */}
      <aside
        className={cn(
          "fixed left-0 top-0 z-40 hidden md:flex h-screen flex-col border-r border-zinc-200 bg-white transition-all duration-300 dark:border-zinc-800 dark:bg-zinc-950",
          collapsed ? "w-16" : "w-64"
        )}
      >
        <div className="flex h-16 items-center justify-between border-b border-zinc-200 px-4 dark:border-zinc-800">
          {!collapsed ? (
            <Link href="/" className="flex items-center gap-2">
              <RsyncLogo size="md" showText />
            </Link>
          ) : (
            <Link href="/" className="mx-auto">
              <RsyncLogo size="md" />
            </Link>
          )}
        </div>

        {navContent()}

        <div className="border-t border-zinc-200 p-2 dark:border-zinc-800">
          <Button
            variant="ghost"
            size="sm"
            // `sidebarCollapsed` is persisted (useUIStore partialize), so the
            // collapsed branch — where this button is a bare chevron — is a state
            // the user carries onto every authenticated page.
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            onClick={() => toggleSidebarCollapsed()}
            className={cn("w-full", collapsed && "px-2")}
          >
            {collapsed ? (
              <ChevronRight className="h-4 w-4" />
            ) : (
              <>
                <ChevronLeft className="h-4 w-4 mr-2" />
                Collapse
              </>
            )}
          </Button>
        </div>
      </aside>

      {/* ── Mobile drawer (< md) ── */}
      {mobileOpen && (
        <div className="fixed inset-0 z-50 md:hidden">
          {/* Backdrop */}
          <div
            className="absolute inset-0 bg-black/50"
            onClick={() => setSidebarOpen(false)}
          />
          {/* Drawer panel */}
          <aside className="absolute left-0 top-0 flex h-full w-72 flex-col border-r border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-950">
            <div className="flex h-16 items-center justify-between border-b border-zinc-200 px-4 dark:border-zinc-800">
              <Link href="/" onClick={() => setSidebarOpen(false)}>
                <RsyncLogo size="md" showText />
              </Link>
              <Button variant="ghost" size="icon-sm" aria-label="Close menu" onClick={() => setSidebarOpen(false)}>
                <X className="h-4 w-4" />
              </Button>
            </div>
            {navContent(true)}
          </aside>
        </div>
      )}
    </TooltipProvider>
  )
}
