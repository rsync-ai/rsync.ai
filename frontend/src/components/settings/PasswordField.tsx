"use client"

import { useState } from "react"
import { Eye, EyeOff } from "lucide-react"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

/**
 * A password input with a show/hide toggle that never covers the text. It
 * prints its own label, unless a settings row labels it (`htmlFor` = `id`).
 */
export function PasswordField({
  id,
  label,
  describedBy,
  toggleNoun,
  value,
  onChange,
  placeholder,
  autoComplete,
}: {
  id: string
  /** Leave out when the row around it holds the label. */
  label?: string
  /** The id of a line that says more about the field. */
  describedBy?: string
  /** What the toggle names, e.g. "current password" → "Show current password". */
  toggleNoun: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
  autoComplete: "current-password" | "new-password"
}) {
  const [shown, setShown] = useState(false)
  return (
    <div className="space-y-2">
      {label && <Label htmlFor={id}>{label}</Label>}
      <div className="relative">
        <Input
          id={id}
          type={shown ? "text" : "password"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          autoComplete={autoComplete}
          aria-describedby={describedBy}
          className="pr-10"
        />
        <button
          type="button"
          aria-label={`${shown ? "Hide" : "Show"} ${toggleNoun}`}
          className="absolute right-1 top-1/2 inline-flex h-8 w-8 -translate-y-1/2 items-center justify-center rounded-md text-zinc-500 hover:bg-zinc-100 hover:text-zinc-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
          onClick={() => setShown((v) => !v)}
        >
          {shown ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
        </button>
      </div>
    </div>
  )
}
