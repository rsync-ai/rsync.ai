"use client"

import Link from "next/link"
import { useParams } from "next/navigation"
import { ChevronLeft } from "lucide-react"

import { PipelinePlanReview } from "@/components/pipeline-plans/PipelinePlanReview"

/**
 * A proposed pipeline, opened from the link plan_pipeline returns to an agent
 * (or from Ask rsync.ai's card). Only the person it was proposed for can load it,
 * and only their click on Create makes the pipeline.
 */
export default function PipelinePlanPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div className="mx-auto max-w-2xl space-y-4">
      <Link
        href="/pipelines"
        className="inline-flex items-center gap-1 text-sm text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200"
      >
        <ChevronLeft className="h-4 w-4" /> Pipelines
      </Link>
      <h1 className="text-2xl font-semibold text-zinc-900 dark:text-white">Review a proposed pipeline</h1>
      <PipelinePlanReview planId={id} />
    </div>
  )
}
