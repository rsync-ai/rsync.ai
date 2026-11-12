import Link from "next/link"
import { cookies } from "next/headers"
import { Plus } from "lucide-react"

import { Button } from "@/components/ui/button"
import { PageHeader } from "@/components/layout/PageHeader"
import { HomeAskBox } from "@/components/home/HomeAskBox"
import { HomeOverview } from "@/components/home/HomeOverview"
import { RecentActivity } from "@/components/home/RecentActivity"
import { ActivityMetricsProvider } from "@/components/runs/ActivityMetricsProvider"
import { RunCharts } from "@/components/runs/RunCharts"
import { FirstRunOnboarding } from "@/components/onboarding/FirstRunOnboarding"
import { API_GATEWAY_URL, API_GATEWAY_URL_INTERNAL } from "@/lib/config/api"

export const dynamic = "force-dynamic"

async function buildCookieHeader(): Promise<string> {
  try {
    const store = await cookies()
    const all = store.getAll()
    if (!all || all.length === 0) return ""
    return all.map((c) => `${c.name}=${c.value}`).join("; ")
  } catch {
    return ""
  }
}

async function fetchGatewayJSON(path: string) {
  const cookieHeader = await buildCookieHeader()
  const headers: Record<string, string> = cookieHeader ? { cookie: cookieHeader } : {}

  // Prefer internal Docker URL on the server, but fall back to localhost for local dev.
  const baseUrls = [API_GATEWAY_URL_INTERNAL, API_GATEWAY_URL].filter(Boolean)

  for (const baseUrl of baseUrls) {
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        cache: "no-store",
        headers,
      }).catch(() => null)

      if (!res || !res.ok) continue
      return await res.json()
    } catch {
      // try next baseUrl
    }
  }

  return null
}

/**
 * The four counts the first-run checklist is about. Only these are read on the
 * server, to decide whether to render the checklist at all; everything else on
 * Home loads in the browser. An unreadable count reads 0 here, which can only
 * show the checklist, and the checklist re-reads and hides itself when done.
 */
async function getOnboardingCounts() {
  const [pipelines, sources, destinations, usage] = await Promise.all([
    fetchGatewayJSON("/api/v1/pipelines?limit=1"),
    fetchGatewayJSON("/api/v1/connections?type=source"),
    fetchGatewayJSON("/api/v1/connections?type=destination"),
    fetchGatewayJSON("/api/v1/usage"),
  ])
  return {
    pipelineCount: Number(pipelines?.total ?? pipelines?.pipelines?.length ?? 0),
    sourceCount: Number(sources?.total ?? sources?.connections?.length ?? 0),
    destinationCount: Number(destinations?.total ?? destinations?.connections?.length ?? 0),
    // NL→SQL queries run this month (onboarding step 4 = "ask a question").
    queryCount: Number(usage?.queries_used ?? 0),
  }
}

/**
 * Home: one box to ask or build, what the workspace has and what needs someone,
 * the run charts, and the newest runs. Each list lives on its own page
 * (Pipelines, Scheduled queries, Workflows, Runs); Home only counts, charts and
 * links. The charts moved here from Runs (dashboards-design.md decision 11).
 */
export default async function HomePage() {
  const counts = await getOnboardingCounts()
  const onboarded =
    counts.sourceCount > 0 && counts.destinationCount > 0 && counts.pipelineCount > 0 && counts.queryCount > 0

  return (
    <div className="space-y-6">
      <PageHeader heading="Home" description="Your pipelines, models and workflows at a glance.">
        <Button asChild className="gap-2 bg-violet-600 text-white hover:bg-violet-700">
          <Link href="/chat">
            <Plus className="h-4 w-4" aria-hidden />
            New pipeline
          </Link>
        </Button>
      </PageHeader>

      {/* The activation checklist. It re-reads the counts in the browser and
          hides itself once every step is done, so an SSR read that failed (and
          so read 0) cannot keep it on screen for an established workspace. */}
      {!onboarded && <FirstRunOnboarding initial={counts} />}

      <HomeAskBox />
      {/* One metrics read for the cards' trend lines and the charts. */}
      <ActivityMetricsProvider>
        <HomeOverview />
        <section aria-labelledby="home-activity-heading">
          <RunCharts
            heading={
              <h2 id="home-activity-heading" className="text-base font-semibold text-zinc-900 dark:text-white">
                Activity
              </h2>
            }
          />
        </section>
      </ActivityMetricsProvider>
      <RecentActivity />
    </div>
  )
}
