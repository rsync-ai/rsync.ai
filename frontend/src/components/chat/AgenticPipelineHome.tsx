'use client';

import { useEffect } from 'react';
import Link from 'next/link';
import { motion } from 'framer-motion';
import { Database, Plus, Sparkles } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { useHomeData } from '@/components/chat/home/useHomeData';
import { buildExamplePrompts, buildQuickPipelines } from '@/components/chat/home/suggestions';
import { QuickPipelines } from '@/components/chat/home/QuickPipelines';
import { HomeSkeleton } from '@/components/chat/home/HomeSkeleton';
import { YourConnections } from '@/components/chat/home/YourConnections';

interface AgenticPipelineHomeProps {
  onSubmit: (intent: string) => void;
  /** Writes an example into the chat's box below, to edit before sending, and focuses it. */
  onPrefill: (text: string) => void;
  /** Focuses the chat's box (the Custom pipeline card). */
  onCustom: () => void;
  /** The example Tab writes into the empty box: the first, grounded in the user's connections. */
  onSuggestion?: (suggestion: string | undefined) => void;
}

// Short and small: a longer fade left the hero half-drawn in screenshots. The
// root MotionConfig drops the movement for users who ask for reduced motion.
const fadeUp = (delay: number) => ({
  initial: { opacity: 0, y: 6 },
  animate: { opacity: 1, y: 0 },
  transition: { delay, duration: 0.2 },
});

/**
 * The pipeline chat's first screen: examples and the pipelines the user's
 * connections can make. The chat's own box below is the only place to type: this
 * screen had a second box of its own, and a user saw two. Only for creating a
 * pipeline; existing pipelines are on Pipelines, their runs on Runs, connections
 * on Connections.
 *
 * Centred in the space above the box: it fills the chat's scroll area, which is
 * a size container while this screen shows (100cqh is that area's height).
 */
export function AgenticPipelineHome({ onSubmit, onPrefill, onCustom, onSuggestion }: AgenticPipelineHomeProps) {
  const { connections, loading, loadError, fetchData } = useHomeData();

  const quickPipelines = buildQuickPipelines(connections);
  const examplePrompts = buildExamplePrompts(quickPipelines);
  const firstExample = examplePrompts[0];

  useEffect(() => {
    onSuggestion?.(firstExample);
  }, [firstExample, onSuggestion]);

  return (
    <div className="flex min-h-[100cqh] flex-col items-center justify-center px-4 py-8 @md:py-10">
      <div className="w-full max-w-5xl space-y-8">
        <motion.div {...fadeUp(0)} className="w-full max-w-2xl mx-auto text-center">
          <div className="inline-flex items-center justify-center w-12 h-12 rounded-2xl bg-gradient-to-br from-violet-500 to-purple-600 text-white mb-3 shadow-lg shadow-violet-500/20">
            <Sparkles className="w-6 h-6" aria-hidden />
          </div>
          <h1 className="text-3xl font-bold tracking-tight text-gray-900 dark:text-white mb-2">New pipeline</h1>
          <p className="text-gray-500 dark:text-gray-400 max-w-md mx-auto">
            Describe it in plain English in the box below, or start from one of these.
          </p>
          {examplePrompts.length > 0 && (
            <div className="mt-4 flex flex-wrap items-center justify-center gap-2">
              <span className="text-xs text-gray-500 dark:text-gray-400">Try:</span>
              {examplePrompts.map((example) => (
                <button
                  key={example}
                  type="button"
                  // Prefill rather than send, so the user can edit before starting.
                  onClick={() => onPrefill(example)}
                  className="rounded-full border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 px-3 py-1 text-xs text-gray-600 dark:text-gray-300 hover:border-violet-300 hover:text-violet-700 dark:hover:border-violet-600 dark:hover:text-violet-300 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500"
                >
                  {example}
                </button>
              ))}
            </div>
          )}
        </motion.div>

        {/* Until connections load, the generic examples would flash "add a connection" at a user who has some. */}
        {loading ? (
          <HomeSkeleton />
        ) : (
          <motion.div {...fadeUp(0.05)} className="space-y-8">
            <YourConnections connections={connections} onPrefill={onPrefill} />
            <QuickPipelines
              pipelines={quickPipelines}
              onPick={(source, dest) => onSubmit(`sync ${source} to ${dest}`)}
              onCustom={onCustom}
            />
          </motion.div>
        )}

        {/* Read failed: unknown, not empty */}
        {!loading && loadError && (
          <motion.div
            {...fadeUp(0.1)}
            className="w-full max-w-md mx-auto text-center p-8 bg-amber-50 dark:bg-amber-950/30 rounded-2xl border-2 border-dashed border-amber-300 dark:border-amber-900"
          >
            <Database className="w-12 h-12 text-amber-400 mx-auto mb-4" aria-hidden />
            <h3 className="font-semibold text-gray-900 dark:text-white mb-2">Could not load your workspace</h3>
            <p className="text-sm text-gray-500 dark:text-gray-400 mb-4">
              {loadError}. Your connections are unknown right now — this is not an empty workspace.
            </p>
            <Button variant="outline" onClick={() => void fetchData()}>
              Retry
            </Button>
          </motion.div>
        )}

        {/* Empty State - No Connections */}
        {!loading && !loadError && connections.length === 0 && (
          <motion.div
            {...fadeUp(0.1)}
            className="w-full max-w-md mx-auto text-center p-8 bg-gray-50 dark:bg-gray-800/50 rounded-2xl border-2 border-dashed border-gray-200 dark:border-gray-700"
          >
            <Database className="w-12 h-12 text-gray-300 dark:text-gray-600 mx-auto mb-4" aria-hidden />
            <h3 className="font-semibold text-gray-900 dark:text-white mb-2">No connections yet</h3>
            <p className="text-sm text-gray-500 dark:text-gray-400 mb-4">
              Set up your first source and destination to start building pipelines
            </p>
            <Button asChild className="bg-violet-600 hover:bg-violet-700">
              <Link href="/connections/new">
                <Plus className="w-4 h-4 mr-2" aria-hidden />
                Create connection
              </Link>
            </Button>
          </motion.div>
        )}
      </div>
    </div>
  );
}
