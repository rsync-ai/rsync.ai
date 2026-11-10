import { cn } from "@/lib/utils"

interface PageHeaderProps {
  heading: string
  /** Sits beside the title, e.g. the caller's role on a settings page. */
  badge?: React.ReactNode
  description?: React.ReactNode
  children?: React.ReactNode
  className?: string
}

export function PageHeader({
  heading,
  badge,
  description,
  children,
  className,
}: PageHeaderProps) {
  return (
    // Wraps rather than squeezes: in a narrow page (the assistant docked beside
    // it) the actions drop under the title instead of crushing it.
    <div className={cn("flex flex-wrap items-center justify-between gap-x-4 gap-y-2", className)}>
      <div className="min-w-0 space-y-0.5">
        {badge ? (
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight text-zinc-900 dark:text-white">
              {heading}
            </h1>
            {badge}
          </div>
        ) : (
          <h1 className="text-xl font-semibold tracking-tight text-zinc-900 dark:text-white">
            {heading}
          </h1>
        )}
        {description && (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">
            {description}
          </p>
        )}
      </div>
      {children && <div className="flex flex-wrap items-center gap-2">{children}</div>}
    </div>
  )
}
