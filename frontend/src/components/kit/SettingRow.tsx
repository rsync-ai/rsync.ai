import type { ReactNode } from "react"
import { Label } from "@/components/ui/label"
import { cn } from "@/lib/utils"

/**
 * A stack of settings rows with a line between each. It is the container the
 * rows measure, so they sit label-left, control-right in a wide card and stack
 * in a narrow one, wherever the card is placed.
 */
export function SettingRows({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("@container divide-y divide-zinc-100 dark:divide-zinc-800", className)}>{children}</div>
}

/**
 * One setting: what it is and a line on what it does on the left, its control
 * on the right. With `htmlFor` the name is the control's label and the line is
 * at `${htmlFor}-hint`, for the control's `aria-describedby`.
 */
export function SettingRow({
  label,
  htmlFor,
  hint,
  children,
  className,
}: {
  label: string
  htmlFor?: string
  hint?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        "grid gap-2 py-4 first:pt-0 last:pb-0 @2xl:grid-cols-[minmax(0,15rem)_minmax(0,1fr)] @2xl:gap-6",
        className,
      )}
    >
      <div className="space-y-1">
        {htmlFor ? (
          <Label htmlFor={htmlFor}>{label}</Label>
        ) : (
          <p className="text-sm font-medium leading-none text-zinc-900 dark:text-zinc-100">{label}</p>
        )}
        {hint && (
          <p id={htmlFor ? `${htmlFor}-hint` : undefined} className="text-xs text-zinc-500 dark:text-zinc-400">
            {hint}
          </p>
        )}
      </div>
      <div className="min-w-0">{children}</div>
    </div>
  )
}
