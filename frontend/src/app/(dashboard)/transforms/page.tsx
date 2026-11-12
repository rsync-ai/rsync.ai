import { redirect } from "next/navigation"

export const dynamic = "force-dynamic"

// Legacy route. The Transform Builder that lived here was a second screen for
// one pipeline's transforms, with its own pipeline picker; a pipeline's rules
// are now added and edited in its Transforms tab, where they are shown. Kept as
// a redirect so old links and bookmarks still land on the rules they meant.
export default async function TransformsRedirectPage({
  searchParams,
}: {
  searchParams?: Promise<{ [key: string]: string | string[] | undefined }>
}) {
  const sp = await searchParams
  const pipeline = typeof sp?.pipeline === "string" ? sp.pipeline.trim() : ""
  redirect(pipeline ? `/pipelines/${encodeURIComponent(pipeline)}?tab=transforms` : "/pipelines")
}
