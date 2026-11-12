"use client"

import Link from "next/link"

import { Badge } from "@/components/ui/badge"
import { checkTypeLabel, storeAssistantDraft, type AssistantChecksDraft } from "@/components/explorer/modelChecks"

/**
 * The data checks Ask rsync.ai drafted (plan_model_checks), shown as a card under the
 * answer. Nothing is saved: the card hands the draft to the model's Checks tab, where the
 * person reviews and adds each check. Whether a check blocks is left to that tab, which
 * knows whether the model's warehouse can hold a table back.
 */
export function ModelChecksDraftReview({ draft }: { draft: AssistantChecksDraft }) {
  const n = draft.drafts.length
  return (
    <section
      aria-label="Drafted data checks"
      className="space-y-2 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-start justify-between gap-2">
        <h3 className="font-semibold text-zinc-900 dark:text-white">
          {n} data {n === 1 ? "check" : "checks"} drafted
        </h3>
        <Badge variant="secondary">Not saved</Badge>
      </header>
      <ul className="space-y-1 text-xs">
        {draft.drafts.map((d, i) => (
          <li key={i} className="break-words text-zinc-700 dark:text-zinc-300">
            <span className="font-medium">{d.name}</span> · {checkTypeLabel(d.type)}
            {d.columns.length > 0 && ` · ${d.columns.join(", ")}`}
          </li>
        ))}
      </ul>
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        Review them on the model&apos;s Checks tab, where Test now runs them against the table. None is added until you
        add it.
      </p>
      <Link
        href={`/models/${encodeURIComponent(draft.modelId)}?tab=checks`}
        onClick={() => storeAssistantDraft(draft)}
        className="inline-block text-xs font-medium text-violet-700 hover:underline dark:text-violet-300"
      >
        Open on the Checks tab
      </Link>
    </section>
  )
}
