"use client"

import { useEffect, useRef, type ReactNode } from "react"
import { X } from "lucide-react"

import { afterWords, isOtherwise } from "@/components/workflows/workflowWords"
import { formatDuration } from "@/lib/duration"
import { conditionStep, type Workflow, type WorkflowTestResult as Result } from "@/lib/api/workflows"

function formatCell(v: unknown): string {
  if (v === null || v === undefined) return ""
  if (typeof v === "object") return JSON.stringify(v)
  return String(v)
}

/**
 * What a Test returned: the rows (to this person only), whether the condition
 * is true, and each channel's message as a run would send it. Nothing was sent
 * and no step ran. With several conditions it says which one is the first
 * that is true, and shows each condition's messages under it, marking the one
 * a run would send. A message that follows no condition is shown under when it
 * sends; a workflow with no query has no rows to show.
 */
export function WorkflowTestResult({
  test,
  workflow: w,
  maxRows,
  onClose,
}: {
  test: Result
  workflow: Workflow
  maxRows?: number
  onClose?: () => void
}) {
  // The result renders under the canvas, out of sight of the Test button: bring each
  // new one into view, or Test looked as if it did nothing. Its top goes to the top,
  // and scroll-mt-24 holds it below the fixed header: "nearest" left its first
  // sentence, the one that says what a run would do, under the header.
  const ref = useRef<HTMLElement>(null)
  useEffect(() => {
    ref.current?.scrollIntoView?.({ behavior: "smooth", block: "start" })
  }, [test])
  const rows = maxRows ? test.rows.slice(0, maxRows) : test.rows
  const branches = conditionStep(w.definition)?.conditions?.length ?? 0
  const several = branches > 1
  const caseMessages = test.messages.filter((m) => m.branch !== undefined)
  const plainSends = w.definition.steps.filter((s) => s.type === "send" && s.case === undefined)
  return (
    <section
      ref={ref}
      aria-label="Test result"
      className="scroll-mt-24 space-y-2 rounded-lg border border-zinc-200 bg-white p-3 text-xs dark:border-zinc-800 dark:bg-zinc-900"
    >
      <div className="flex items-start justify-between gap-2">
        <p className="text-zinc-700 dark:text-zinc-300">
          {typeof test.row_count === "number" && (
            <>
              The query returned {test.row_count}
              {test.truncated ? "+" : ""} {test.row_count === 1 ? "row" : "rows"} in {formatDuration(test.execution_time_ms)}.{" "}
            </>
          )}
          {branches === 0 ? null : test.condition_error ? (
            <span className="text-red-700 dark:text-red-300">{test.condition_error}</span>
          ) : test.condition_met ? (
            <strong className="text-emerald-700 dark:text-emerald-300">
              {!several
                ? "The condition is true: a run now would send this."
                : // Otherwise is not a condition: it sends because none of them is true.
                  isOtherwise(w, test.branch ?? 0)
                  ? "No condition is true, so Otherwise sends its message: a run now would send it."
                  : `Condition ${(test.branch ?? 0) + 1} is the first that is true: a run now would send its message.`}
            </strong>
          ) : (
            <strong className="text-zinc-700 dark:text-zinc-200">
              {several
                ? "No condition is true: a run now would send nothing."
                : "The condition is not true: a run now would send nothing."}
            </strong>
          )}{" "}
          Nothing was sent.
        </p>
        {onClose && (
          <button
            type="button"
            onClick={onClose}
            aria-label="Close the test result"
            className="rounded p-0.5 text-zinc-500 hover:bg-zinc-100 dark:text-zinc-400 dark:hover:bg-zinc-800"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        )}
      </div>
      {rows.length > 0 && (
        <div className="max-h-56 overflow-auto rounded border border-zinc-100 dark:border-zinc-800">
          <table className="w-full text-left">
            <thead className="sticky top-0 bg-zinc-50 dark:bg-zinc-950">
              <tr>
                {test.columns.map((c) => (
                  <th key={c} scope="col" className="px-2 py-1 font-medium text-zinc-600 dark:text-zinc-300">
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                // The rows have no identity of their own and never reorder: the index is their key.
                <tr key={i} className="border-t border-zinc-100 dark:border-zinc-800">
                  {test.columns.map((c) => (
                    <td key={c} className="max-w-[16rem] truncate px-2 py-1 font-mono text-zinc-800 dark:text-zinc-200">
                      {formatCell(r[c])}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
          {rows.length < test.rows.length && (
            <p className="border-t border-zinc-100 px-2 py-1 text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
              {test.rows.length - rows.length} more rows in the full test result.
            </p>
          )}
        </div>
      )}
      {plainSends.map((s) => {
        const messages = test.messages.filter((m) => m.step === s.id)
        return (
          <div key={s.id} role="group" aria-label={`Message step ${s.id}`} className="space-y-1">
            <p className="font-semibold text-zinc-700 dark:text-zinc-200">Sent {afterWords(w, s)}</p>
            {messages.length === 0 ? (
              <p className="text-zinc-500 dark:text-zinc-400">No channel.</p>
            ) : (
              messages.map((m) => <TestMessage key={`${m.step}:${m.kind}:${m.channel}`} message={m} />)
            )}
          </div>
        )
      })}
      {several
        ? Array.from({ length: branches }, (_, b) => {
            const messages = test.messages.filter((m) => m.branch === b)
            const sends = test.condition_met && (test.branch ?? 0) === b
            const title = isOtherwise(w, b) ? "Otherwise" : `Condition ${b + 1}`
            return (
              <div key={b} role="group" aria-label={title} className="space-y-1">
                <p className="font-semibold text-zinc-700 dark:text-zinc-200">
                  {title}
                  {sends && (
                    <span className="ml-1 font-normal text-emerald-700 dark:text-emerald-300">: a run now would send this</span>
                  )}
                </p>
                {messages.length === 0 ? (
                  <p className="text-zinc-500 dark:text-zinc-400">No channel.</p>
                ) : (
                  messages.map((m) => <TestMessage key={`${m.step}:${m.kind}:${m.channel}`} message={m} />)
                )}
              </div>
            )
          })
        : caseMessages.map((m) => <TestMessage key={`${m.step}:${m.kind}:${m.channel}`} message={m} />)}
      {test.notes.map((n, i) => (
        <p key={`${i}:${n}`} className="text-amber-700 dark:text-amber-300">
          {n}
        </p>
      ))}
    </section>
  )
}

function TestMessage({ message: m }: { message: Result["messages"][number] }) {
  return (
    <div className="space-y-1">
      <p className="font-medium text-zinc-600 dark:text-zinc-300">
        {m.kind === "slack" ? "Slack" : "Email"} to {m.channel}
        {m.subject ? `: ${m.subject}` : ""}
      </p>
      {m.kind === "slack" ? (
        <div className="max-h-48 overflow-auto whitespace-pre-wrap rounded bg-zinc-50 p-2 text-xs text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200">
          <SlackText text={m.body} />
        </div>
      ) : (
        <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded bg-zinc-50 p-2 font-mono text-[11px] text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200">
          {m.body}
        </pre>
      )}
    </div>
  )
}

const SLACK_ENTITIES: Record<string, string> = { "&amp;": "&", "&lt;": "<", "&gt;": ">" }
const slackUnescape = (s: string) => s.replace(/&(amp|lt|gt);/g, (e) => SLACK_ENTITIES[e])
// <url|text> and <url>, *bold*, _italic_. The server escapes every < in a value,
// so only its own links match.
const SLACK_INLINE = /<(https?:\/\/[^|>\s]+)(?:\|([^>]+))?>|\*([^*\n]+)\*|_([^_\n]+)_/g

/**
 * A Slack message as Slack shows it. The body is Slack's markup: & < > escaped,
 * *bold*, _italic_, <url|text> links and ``` blocks, which raw read as "&amp;"
 * and stray stars.
 */
export function SlackText({ text }: { text: string }) {
  return (
    <>
      {text.split("```").map((part, i) =>
        i % 2 === 1 ? (
          <code key={i} className="block font-mono text-[11px]">
            {slackUnescape(part.replace(/^\n|\n$/g, ""))}
          </code>
        ) : (
          <span key={i}>{slackInline(part)}</span>
        ),
      )}
    </>
  )
}

function slackInline(s: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0
  for (const m of s.matchAll(SLACK_INLINE)) {
    const at = m.index ?? 0
    // Slack reads _ as italic only at a word's edge: not in a_b_c.
    if (m[4] !== undefined && (/\w/.test(s[at - 1] ?? "") || /\w/.test(s[at + m[0].length] ?? ""))) continue
    if (at > last) out.push(slackUnescape(s.slice(last, at)))
    if (m[1] !== undefined) {
      out.push(
        <a key={at} href={slackUnescape(m[1])} target="_blank" rel="noreferrer" className="text-sky-700 underline dark:text-sky-400">
          {slackUnescape(m[2] ?? m[1])}
        </a>,
      )
    } else if (m[3] !== undefined) {
      out.push(<strong key={at}>{slackInline(m[3])}</strong>)
    } else {
      out.push(<em key={at}>{slackInline(m[4])}</em>)
    }
    last = at + m[0].length
  }
  if (last < s.length) out.push(slackUnescape(s.slice(last)))
  return out
}
