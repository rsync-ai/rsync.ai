'use client';

import { useCallback, useEffect, useState } from 'react';
import { API_ENDPOINTS } from '@/lib/config/api';
import { authFetch } from '@/lib/api/auth-fetch';
import { captureWorkspace, onActiveWorkspaceChange } from '@/lib/workspace/active-workspace';
import type { ConnectionTestFields } from '@/components/connectors/ConnectionTestStatus';

/** Carries the test fields so the chat judges an expired token as Connections does. */
export interface HomeConnection extends ConnectionTestFields {
  id: string;
  name: string;
  connector_type: string;
  type: 'source' | 'destination';
  status: string;
}

/**
 * The pipeline chat's connections, for its connection chips and quick pipelines. Existing pipelines
 * are not read here: they are listed on Pipelines and their runs on Runs.
 */
export function useHomeData() {
  const [connections, setConnections] = useState<HomeConnection[]>([]);
  const [loading, setLoading] = useState(true);
  // The read used to fall through to [] on failure, so an outage rendered as
  // "No connections yet" with a Create button.
  const [loadError, setLoadError] = useState<string | null>(null);

  const fetchData = useCallback(async () => {
    // Drop the response if a switch overtook the request, or the previous
    // workspace's connections land under the new one.
    const isStale = captureWorkspace();
    setLoading(true);
    setLoadError(null);
    try {
      const connRes = await authFetch(API_ENDPOINTS.CONNECTIONS.LIST, { cache: 'no-store' });
      if (isStale()) return;
      if (connRes.ok) {
        const connData = await connRes.json();
        if (isStale()) return;
        setConnections(Array.isArray(connData) ? connData : connData.connections || []);
      } else {
        setLoadError(`The server answered ${connRes.status} for your connections`);
      }
    } catch (error) {
      console.error('Failed to fetch connections:', error);
      if (!isStale()) setLoadError('Could not reach the server');
    } finally {
      if (!isStale()) setLoading(false);
    }
  }, []);

  // Load on mount, and reload on every active-workspace change. Connections are
  // workspace-scoped, so a switch invalidates them: without this the page kept
  // offering the PREVIOUS workspace's source → destination pairs under the new
  // workspace's name in the header. Same-tab and cross-tab.
  useEffect(() => {
    void fetchData();
    return onActiveWorkspaceChange(() => {
      // Clear first: the refetch is async, and showing the old tenant's pairs
      // until it lands is the bug, not a loading state.
      setConnections([]);
      void fetchData();
    });
  }, [fetchData]);

  return { connections, loading, loadError, fetchData };
}
