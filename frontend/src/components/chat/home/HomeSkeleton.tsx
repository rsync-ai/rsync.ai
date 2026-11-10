import { Skeleton } from '@/components/ui/skeleton';

/** Placeholder for the quick pipeline cards while the user's connections load. */
export function HomeSkeleton() {
  return (
    <div data-testid="home-skeleton" role="status" aria-busy="true" aria-label="Loading your connections" className="w-full space-y-3">
      <Skeleton className="h-4 w-32" />
      <div className="grid grid-cols-1 @md:grid-cols-2 @3xl:grid-cols-3 gap-3">
        {[0, 1, 2].map((i) => (
          <div key={i} className="flex items-center gap-3 p-4 rounded-xl border border-gray-200 dark:border-gray-700">
            <Skeleton className="h-8 w-16" />
            <div className="flex-1 space-y-2">
              <Skeleton className="h-3.5 w-28" />
              <Skeleton className="h-3 w-36" />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
