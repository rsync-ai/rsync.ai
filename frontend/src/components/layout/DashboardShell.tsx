"use client"

import { usePathname } from "next/navigation"
import { cn } from "@/lib/utils"
import { useDockWidthVar } from "@/lib/assistant/use-dock-width"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { useUIStore } from "@/lib/store/useUIStore"

/**
 * How the shell frames the page it hosts.
 *
 * - `page` — a padded column that scrolls with the window: lists, settings and
 *   detail pages.
 * - `workspace` — edge to edge, exactly as tall as the window below the header.
 *   A tool's panes (schema tree, editor, results, canvas) split that whole area
 *   between them, divided by borders, and each pane scrolls on its own.
 *
 * Picked from the route here rather than by each page, so the server render
 * already has the right frame and nothing moves when the page hydrates.
 */
export type ShellLayout = "page" | "workspace"

// Exact paths: anything under one of them (a detail page) is an ordinary page.
const WORKSPACE_ROUTES: ReadonlySet<string> = new Set([
  "/explorer",
  "/explorer/lineage",
  "/chat",
])

export function shellLayoutFor(pathname: string | null | undefined): ShellLayout {
  if (!pathname) return "page"
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname
  return WORKSPACE_ROUTES.has(path) ? "workspace" : "page"
}

interface DashboardShellProps {
  children: React.ReactNode
  /** Full-width strips (email verification, plan) above the page, outside its padding. */
  banners?: React.ReactNode
  className?: string
}

export function DashboardShell({ children, banners, className }: DashboardShellProps) {
  const collapsed = useUIStore((state) => state.sidebarCollapsed)
  // On a wide screen the docked assistant sits beside the page rather than over
  // it: the page gives up the panel's width, which the user can drag. Both read
  // --assistant-dock-width, set on <html> here (lib/assistant/use-dock-width.ts).
  const docked = useAssistantStore((state) => state.open && !state.expanded)
  // While the edge is dragged the padding follows the pointer, not a 300 ms tween.
  const resizing = useAssistantStore((state) => state.dockResizing)
  useDockWidthVar()
  const layout = shellLayoutFor(usePathname())
  const workspace = layout === "workspace"

  return (
    <main
      data-layout={layout}
      className={cn(
        "bg-zinc-50 pt-16 dark:bg-zinc-900",
        !resizing && "transition-[padding] duration-300",
        "pl-0",
        collapsed ? "md:pl-16" : "md:pl-64",
        docked && "xl:pr-(--assistant-dock-width)",
        workspace ? "flex h-dvh flex-col" : "min-h-screen",
        className
      )}
    >
      {banners}
      {/*
        The page lays itself out by the width of this box, not the window's: the
        sidebar and the docked assistant both take from it, and a viewport
        breakpoint sees neither. So page content uses container variants
        (`@3xl:grid-cols-3`), never `lg:` — pinned by layout-breakpoints.test.ts.
        `container-type: inline-size` is not a containing block for `position:
        fixed`, so a fixed child (a toast, a FAB) still anchors to the window —
        measured in Chromium. A page clips sideways rather than scroll the window:
        `clip`, not `hidden`, so the frame is no scroll container and a sticky
        header inside it still sticks to the window.
      */}
      <div
        data-testid="page-frame"
        className={cn(
          "@container w-full",
          workspace ? "flex min-h-0 flex-1 flex-col overflow-auto" : "overflow-x-clip p-4 sm:p-5"
        )}
      >
        {children}
      </div>
    </main>
  )
}
