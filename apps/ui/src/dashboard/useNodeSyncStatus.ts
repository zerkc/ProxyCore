import { useCallback, useEffect, useRef, useState } from "react";
import { getTopologySync } from "./topologySync";
import type { TopologySyncStatus } from "./types";

export type NodeSyncState = {
  status: TopologySyncStatus | undefined;
  loadFailed: boolean;
  reload: () => Promise<void>;
};

/** Fetches the topology sync status while `enabled`; `reload` refreshes it in place. */
export function useNodeSyncStatus(enabled: boolean, key?: string): NodeSyncState {
  const [status, setStatus] = useState<TopologySyncStatus | undefined>();
  const [loadFailed, setLoadFailed] = useState(false);
  const mounted = useRef(true);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await getTopologySync(signal);
      if (!mounted.current || signal?.aborted) return;
      setStatus(next);
      setLoadFailed(false);
    } catch {
      if (mounted.current && !signal?.aborted) setLoadFailed(true);
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    if (!enabled) return undefined;
    const controller = new AbortController();
    void load(controller.signal);
    return () => {
      mounted.current = false;
      controller.abort();
    };
  }, [enabled, key, load]);

  const reload = useCallback(() => load(), [load]);

  return { status, loadFailed, reload };
}
