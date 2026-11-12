"use client"

import dynamic from "next/dynamic"

import type { DashboardGridEditorProps } from "@/components/dashboards/DashboardGridEditor"

const LazyGridEditor = dynamic(
  () => import("@/components/dashboards/DashboardGridEditor").then((m) => m.DashboardGridEditor),
  { ssr: false, loading: () => <div className="h-64 w-full animate-pulse rounded bg-zinc-100 dark:bg-zinc-900" /> },
)

/** The editor's grid in its own chunk: a viewer never downloads react-grid-layout. */
export function DashboardGridEditorNoSSR(props: DashboardGridEditorProps) {
  return <LazyGridEditor {...props} />
}
