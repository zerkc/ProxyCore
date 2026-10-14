import { useCallback, useEffect, useRef, useState } from "react";
import { getTopologySync, retryTopologySync, shortHash, TopologySyncError } from "./topologySync";
import type { TopologyRole, TopologySyncStatus } from "./types";

function formatDate(value?: string) {
  if (!value) return "never";
  const timestamp = Date.parse(value);
  return Number.isNaN(timestamp) ? "—" : new Date(timestamp).toISOString();
}

function retryError(caught: unknown) {
  if (caught instanceof TopologySyncError) {
    if (caught.kind === "not-available") return "Sync is not available right now: this installation is not a bound NODE, or a sync cycle is already running.";
    if (caught.kind === "sync-failed") return "The sync attempt failed. Check the last error below and try again.";
    if (caught.kind === "forbidden") return "Owner access is required to trigger a sync.";
    if (caught.kind === "network") return "The sync service could not be reached. Check the connection and try again.";
  }
  return "The sync could not be started. Try again.";
}

export function NodeSyncStatus({ role }: { role?: TopologyRole }) {
  const visible = role === "node" || role === "primary" || role === "primary-with-nodes";
  const isNode = role === "node";
  const [status, setStatus] = useState<TopologySyncStatus | undefined>();
  const [loadFailed, setLoadFailed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
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
    if (!visible) return undefined;
    const controller = new AbortController();
    void load(controller.signal);
    return () => {
      mounted.current = false;
      controller.abort();
    };
  }, [visible, role, load]);

  async function syncNow() {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await retryTopologySync();
    } catch (caught) {
      if (mounted.current) setError(retryError(caught));
    }
    await load();
    if (mounted.current) setBusy(false);
  }

  if (!visible) return null;

  return (
    <section className="mt-8 space-y-4 border-t border-line/80 pt-6" aria-labelledby="node-sync-title" data-testid="node-sync-status">
      <header className="border-b border-line/80 pb-4">
        <p className="pc-eyebrow pc-eyebrow-signal">configuration sync</p>
        <h2 id="node-sync-title" className="pc-title mt-2 text-2xl text-mist">{isNode ? "Sync with PRIMARY" : "Published configuration"}</h2>
      </header>
      {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
      {loadFailed && !status ? <p className="text-sm text-faint" role="status">Sync status is unavailable.</p> : null}
      {status ? (isNode ? <NodeBody status={status} busy={busy} onSync={() => void syncNow()} /> : <PrimaryBody status={status} />) : null}
    </section>
  );
}

function badge(status: TopologySyncStatus) {
  if (!status.appliedHash) return "Never synced";
  return status.outdated ? "Outdated" : "In sync";
}

function NodeBody({ status, busy, onSync }: { status: TopologySyncStatus; busy: boolean; onSync: () => void }) {
  const label = badge(status);
  const fields: Array<[string, string]> = [
    ["applied hash", shortHash(status.appliedHash)],
    ["primary hash", shortHash(status.primaryHash)],
    ["last sync", formatDate(status.lastSyncAt)],
  ];
  return (
    <div className="space-y-4">
      <p><span className={label === "In sync" ? "pc-eyebrow pc-eyebrow-signal" : "pc-eyebrow"} data-testid="node-sync-badge">{label}</span></p>
      <dl className="grid gap-4 border-y border-line/80 px-1 py-5 sm:grid-cols-3">
        {fields.map(([name, value]) => <div key={name}><dt className="pc-eyebrow">{name}</dt><dd className="mt-1 break-all font-mono text-sm text-mist">{value}</dd></div>)}
      </dl>
      {status.lastError ? <p className="text-sm leading-6 text-mute" data-testid="node-sync-last-error">Last error: {status.lastError}</p> : null}
      {!status.bound ? <p className="text-sm leading-6 text-mute">This NODE is not bound to a PRIMARY, so it cannot sync.</p> : null}
      <button type="button" className="pc-btn" data-testid="node-sync-now" disabled={busy || !status.bound} onClick={onSync}>{busy ? "Syncing…" : "Sync now"}</button>
    </div>
  );
}

function PrimaryBody({ status }: { status: TopologySyncStatus }) {
  return (
    <dl className="grid gap-4 border-y border-line/80 px-1 py-5">
      <div><dt className="pc-eyebrow">published hash</dt><dd className="mt-1 break-all font-mono text-sm text-mist" data-testid="primary-published-hash">{shortHash(status.primaryHash)}</dd></div>
    </dl>
  );
}
