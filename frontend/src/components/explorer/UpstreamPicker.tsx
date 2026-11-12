"use client"

import { type KeyboardEvent, type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react"
import { Plus, Search, X } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

/** One thing a model can be told to rebuild after: a pipeline, or another model. */
export interface UpstreamOption {
  kind: "pipeline" | "model"
  id: string
  name: string
  /** Says which one this is: a pipeline's "source → destination · CDC", what a model builds. */
  detail?: string
  /**
   * The connection whose tables this producer writes: a pipeline's destination, a
   * model's own connection. Absent for a selection whose producer left its list.
   */
  connectionId?: string
}

/**
 * Identity of one upstream. The kind is part of it rather than decoration: pipelines
 * and models are separate tables with independent id spaces, so a set keyed on the id
 * alone would let a selection in one list cancel one in the other.
 */
export function upstreamKey(u: { kind: string; id: string }): string {
  return `${u.kind}:${u.id}`
}

interface Section {
  key: string
  title: string
  options: UpstreamOption[]
}

/**
 * The "Rebuild after" control: what is chosen, as removable chips, and a panel to
 * choose more from.
 *
 * It replaced one long box of every pipeline and model in the workspace. A model reads
 * the tables of its own connection, so the producers worth offering first are the
 * pipelines that load that connection and the models built on it; the rest are behind
 * one toggle, and search covers everything. A row says which one it is (source and
 * destination, or what the model builds), and two with the same name also show their
 * id, so the choice never comes down to guessing between identical labels.
 *
 * The panel sits in the dialog's flow rather than floating over it: the dialog already
 * scrolls, and a popover inside a scrolling dialog clips. It starts open when nothing
 * is chosen — choosing is the only thing to do then — and closed when a trigger already
 * has upstreams, which the chips show.
 */
export function UpstreamPicker({
  labelId,
  descriptionId,
  options,
  selected,
  suggestedKeys,
  connectionId,
  readOnly,
  atCap,
  onToggle,
}: {
  /** The id of the visible label that names this control. */
  labelId: string
  /** The id of the line under the label that says what choosing one means. */
  descriptionId?: string
  /** Everything that can be chosen, plus any chosen upstream no list returns any more. */
  options: UpstreamOption[]
  /** The chosen upstreams, in the order they were chosen. */
  selected: UpstreamOption[]
  /** Producers inferred from the query's SQL, marked in the panel. Never pre-selected. */
  suggestedKeys: Set<string>
  /** The model's own connection. Unknown → one list per kind, no connection grouping. */
  connectionId?: string
  readOnly: boolean
  /** At the server's cap: unchosen rows are disabled, chosen ones stay removable. */
  atCap: boolean
  onToggle: (o: UpstreamOption) => void
}): ReactNode {
  const scope = useId()
  const panelId = `${scope}panel`
  const [open, setOpen] = useState(() => selected.length === 0 && !readOnly)
  const [query, setQuery] = useState("")
  const [showOthers, setShowOthers] = useState(false)
  const toggleRef = useRef<HTMLButtonElement>(null)
  // Focus moves into the search box only when the user opened the panel. On the open it
  // starts with, the user is still on the cadence select, and taking focus from there
  // would be a jump nobody asked for.
  const focusSearch = useRef(false)
  const chipsRef = useRef<HTMLUListElement>(null)
  // A removed chip takes its button, and the focus on it, with it. Focus goes to the
  // chip that took its place (the one before it at the end of the list), or to the
  // Add button when none is left, instead of dropping to the page.
  const refocusChip = useRef<number | null>(null)
  useEffect(() => {
    const at = refocusChip.current
    if (at === null) return
    refocusChip.current = null
    const buttons = chipsRef.current?.querySelectorAll<HTMLButtonElement>("button") ?? []
    const next = buttons.length > 0 ? buttons[Math.min(at, buttons.length - 1)] : toggleRef.current
    next?.focus()
  }, [selected])

  const selectedKeys = useMemo(() => new Set(selected.map(upstreamKey)), [selected])

  // Two producers with one name are told apart by their id, not left for the user to
  // guess between. Counted per kind: a pipeline and a model may share a name, and the
  // section they sit in already separates them.
  const duplicateNames = useMemo(() => {
    const counts = new Map<string, number>()
    for (const o of options) {
      const k = `${o.kind}:${o.name.toLowerCase()}`
      counts.set(k, (counts.get(k) ?? 0) + 1)
    }
    return new Set(Array.from(counts).filter(([, n]) => n > 1).map(([k]) => k))
  }, [options])

  const describe = (o: UpstreamOption): string => {
    const parts: string[] = []
    if (o.detail) parts.push(o.detail)
    if (duplicateNames.has(`${o.kind}:${o.name.toLowerCase()}`)) parts.push(`#${o.id.slice(0, 8)}`)
    return parts.join(" · ")
  }

  const needle = query.trim().toLowerCase()
  const matches = (o: UpstreamOption) =>
    needle === "" || o.name.toLowerCase().includes(needle) || describe(o).toLowerCase().includes(needle)
  // Suggested first, otherwise in the order the lists arrived (newest pipeline first).
  const ordered = (list: UpstreamOption[]) => [
    ...list.filter((o) => suggestedKeys.has(upstreamKey(o))),
    ...list.filter((o) => !suggestedKeys.has(upstreamKey(o))),
  ]

  // Grouped by connection only when that splits something: with no connection known,
  // or nothing on it, "this connection" would be an empty heading over a hidden list.
  const scoped = Boolean(connectionId) && options.some((o) => o.connectionId === connectionId)
  const local = scoped ? options.filter((o) => o.connectionId === connectionId) : options
  const others = scoped ? options.filter((o) => o.connectionId !== connectionId) : []
  const searching = needle !== ""
  const othersVisible = searching || showOthers

  const sections: Section[] = [
    {
      key: "pipeline-local",
      title: scoped ? "Pipelines loading this connection" : "Pipelines",
      options: ordered(local.filter((o) => o.kind === "pipeline" && matches(o))),
    },
    {
      key: "model-local",
      title: scoped ? "Models on this connection" : "Models",
      options: ordered(local.filter((o) => o.kind === "model" && matches(o))),
    },
    ...(othersVisible
      ? [
          {
            key: "pipeline-other",
            title: "Pipelines loading other connections",
            options: ordered(others.filter((o) => o.kind === "pipeline" && matches(o))),
          },
          {
            key: "model-other",
            title: "Models on other connections",
            options: ordered(others.filter((o) => o.kind === "model" && matches(o))),
          },
        ]
      : []),
  ].filter((s) => s.options.length > 0)

  const close = () => {
    setOpen(false)
    setQuery("")
    toggleRef.current?.focus()
  }

  // The dialog closes on an Escape nothing else claimed (ui/dialog.tsx). While the
  // panel is open, the first Escape is the panel's from anywhere in the picker: the
  // panel starts open without taking focus, so focus is as likely on the Add button as
  // in the search box. Once it is closed, Escape is left to the dialog.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "Escape" || !open) return
    e.preventDefault()
    close()
  }

  return (
    <div role="group" aria-labelledby={labelId} aria-describedby={descriptionId} className="space-y-2" onKeyDown={onKeyDown}>
      {selected.length === 0 ? (
        <p className="text-xs text-zinc-500 dark:text-zinc-400">Nothing chosen yet.</p>
      ) : (
        <ul ref={chipsRef} aria-label="Chosen" className="flex flex-wrap gap-1.5">
          {selected.map((o, i) => {
            const detail = describe(o)
            return (
              <li
                key={upstreamKey(o)}
                className="inline-flex max-w-full items-center gap-1 rounded-full border border-zinc-200 bg-zinc-50 py-0.5 pl-2.5 pr-1 text-xs dark:border-zinc-700 dark:bg-zinc-800/60"
                title={detail || undefined}
              >
                <span className="text-zinc-500 dark:text-zinc-400">
                  {o.kind === "model" ? "Model" : "Pipeline"}
                </span>
                <span className="truncate font-medium">{o.name}</span>
                {!readOnly && (
                  <button
                    type="button"
                    aria-label={`Remove ${o.name}`}
                    className="rounded-full p-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:text-zinc-400 dark:hover:bg-zinc-700 dark:hover:text-zinc-100"
                    onClick={() => {
                      refocusChip.current = i
                      onToggle(o)
                    }}
                  >
                    <X className="h-3 w-3" aria-hidden />
                  </button>
                )}
              </li>
            )
          })}
        </ul>
      )}

      {!readOnly && (
        <Button
          ref={toggleRef}
          type="button"
          variant="outline"
          size="sm"
          className="h-8 text-xs"
          aria-expanded={open}
          aria-controls={open ? panelId : undefined}
          onClick={() => {
            if (open) return close()
            focusSearch.current = true
            setOpen(true)
          }}
        >
          <Plus className="mr-1 h-3.5 w-3.5" aria-hidden />
          {selected.length === 0 ? "Choose a pipeline or model" : "Add a pipeline or model"}
        </Button>
      )}

      {open && !readOnly && (
        <div id={panelId} className="space-y-2 rounded-md border p-2">
          <div className="relative">
            <Search
              className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-zinc-400"
              aria-hidden
            />
            <Input
              type="search"
              aria-label="Search pipelines and models"
              placeholder="Search by name, connection or table"
              className="h-8 pl-7 text-xs"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              ref={(el) => {
                if (el && focusSearch.current) {
                  focusSearch.current = false
                  el.focus()
                }
              }}
            />
          </div>

          <div className="max-h-64 space-y-3 overflow-y-auto">
            {sections.length === 0 ? (
              <p className="px-1 text-xs text-zinc-500 dark:text-zinc-400">
                {searching ? `Nothing matches “${query.trim()}”.` : "Nothing on this connection to follow."}
              </p>
            ) : (
              sections.map((section) => (
                <div
                  key={section.key}
                  role="group"
                  aria-labelledby={`${scope}${section.key}`}
                  className="space-y-1"
                >
                  <p
                    id={`${scope}${section.key}`}
                    className="text-[11px] font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400"
                  >
                    {section.title}
                  </p>
                  {section.options.map((o) => {
                    const key = upstreamKey(o)
                    const checked = selectedKeys.has(key)
                    const detail = describe(o)
                    const suggested = suggestedKeys.has(key)
                    const rowId = `${scope}${key}`
                    return (
                      <div key={key} className="flex items-start gap-2 rounded px-1 py-0.5">
                        <Checkbox
                          id={rowId}
                          className="mt-0.5"
                          checked={checked}
                          // Disabled at the cap rather than refused on click: a box that
                          // ticks and then unticks itself reads as a bug.
                          disabled={!checked && atCap}
                          aria-describedby={detail || suggested ? `${rowId}-detail` : undefined}
                          onCheckedChange={() => onToggle(o)}
                        />
                        <div className="min-w-0">
                          <Label htmlFor={rowId} className="block truncate font-normal">
                            {o.name}
                          </Label>
                          {(detail || suggested) && (
                            // Wraps rather than truncates: the #id that tells two
                            // same-named rows apart comes last and must not be cut off.
                            <p id={`${rowId}-detail`} className="break-words text-xs text-zinc-500 dark:text-zinc-400">
                              {suggested && (
                                <span className="text-violet-600 dark:text-violet-400">Suggested</span>
                              )}
                              {/* Outside the span: an element's text is trimmed when it is
                                  read out, so a separator inside it ran into the detail. */}
                              {suggested && detail ? " · " : ""}
                              {detail}
                            </p>
                          )}
                        </div>
                      </div>
                    )
                  })}
                </div>
              ))
            )}
          </div>

          <div className="flex items-center justify-between gap-2">
            {others.length > 0 && !searching ? (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-xs"
                aria-expanded={showOthers}
                onClick={() => setShowOthers((v) => !v)}
              >
                {showOthers ? "Hide other connections" : `Show ${others.length} from other connections`}
              </Button>
            ) : (
              <span />
            )}
            <Button type="button" variant="secondary" size="sm" className="h-7 text-xs" onClick={close}>
              Done
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
