'use client';

import Link from 'next/link';
import { ConnectionLogo } from '@/components/connectors/ConnectionLogo';
import { connectionTestState } from '@/components/connectors/ConnectionTestStatus';
import { normalizeConnectorForLogo, shortConnectorName } from './suggestions';
import type { HomeConnection } from './useHomeData';

/** Chips shown before the rest collapse into "+N more". */
export const MAX_CONNECTION_CHIPS = 8;

const chip =
  'inline-flex items-center gap-2 rounded-full border bg-white py-1 pl-1 pr-3 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:bg-gray-900';

/**
 * The text a chip writes into the chat's box. The connector name is what the
 * planner's resolver matches (it matches connector types, not connection names);
 * the connection's own name tells the user which one they picked.
 */
export function connectionPrompt(c: HomeConnection): string {
  const label = `${c.name} (${shortConnectorName(c.connector_type)})`;
  return c.type === 'destination' ? `Load into ${label} from ` : `Sync ${label} to `;
}

/**
 * Sources first, then destinations. Each kind keeps half the chips and a kind
 * with fewer gives the rest to the other, so many sources never hide every
 * destination.
 */
function pickChips(connections: HomeConnection[]): HomeConnection[] {
  const sources = connections.filter((c) => c.type !== 'destination');
  const destinations = connections.filter((c) => c.type === 'destination');
  const half = MAX_CONNECTION_CHIPS / 2;
  const sourceCount = Math.min(sources.length, Math.max(half, MAX_CONNECTION_CHIPS - destinations.length));
  return [...sources.slice(0, sourceCount), ...destinations.slice(0, MAX_CONNECTION_CHIPS - sourceCount)];
}

/**
 * The user's connections above the starter cards. A chip starts a prompt from
 * that connection; one whose token expired links to its page to reconnect,
 * judged by the same rule as the Connections badge.
 */
export function YourConnections({
  connections,
  onPrefill,
}: {
  connections: HomeConnection[];
  onPrefill: (text: string) => void;
}) {
  if (connections.length === 0) return null;
  const shown = pickChips(connections);
  const more = connections.length - shown.length;

  return (
    <section aria-labelledby="your-connections-heading" className="w-full">
      <div className="mb-3 flex flex-wrap items-baseline gap-x-2">
        <h2 id="your-connections-heading" className="text-sm font-semibold text-gray-700 dark:text-gray-300">
          Your connections
        </h2>
        <span className="text-xs text-gray-500 dark:text-gray-400">Pick one to start from it</span>
      </div>
      <ul className="flex flex-wrap gap-2">
        {shown.map((c) => {
          const logo = <ConnectionLogo connectorType={normalizeConnectorForLogo(c.connector_type)} size="sm" />;
          return (
            <li key={c.id}>
              {connectionTestState(c) === 'expired' ? (
                <Link
                  href={`/connections/${c.id}?type=${c.type}`}
                  className={`${chip} border-amber-300 hover:bg-amber-50 dark:border-amber-800 dark:hover:bg-amber-950/30`}
                >
                  {logo}
                  <span className="max-w-40 truncate text-gray-700 dark:text-gray-200">{c.name}</span>
                  <span className="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800 dark:bg-amber-900/40 dark:text-amber-200">
                    Reconnect
                  </span>
                </Link>
              ) : (
                <button
                  type="button"
                  onClick={() => onPrefill(connectionPrompt(c))}
                  className={`${chip} border-gray-200 hover:border-violet-300 dark:border-gray-700 dark:hover:border-violet-600`}
                >
                  {logo}
                  <span className="max-w-40 truncate text-gray-700 dark:text-gray-200">{c.name}</span>
                </button>
              )}
            </li>
          );
        })}
        {more > 0 && (
          <li className="flex items-center">
            <Link
              href="/connections"
              className="rounded-full px-3 py-1 text-sm text-gray-500 underline-offset-2 hover:underline dark:text-gray-400"
            >
              +{more} more
            </Link>
          </li>
        )}
      </ul>
    </section>
  );
}
