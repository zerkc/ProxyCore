import { useCallback, useEffect, useRef, useState } from "react";
import { listTopologyNodes, TopologyApiError, unlinkTopologyNode } from "../api";
import type { TopologyNode, TopologyNodeList, TopologyRole } from "./types";

/**
 * A linked node that has not reported for longer than this is shown as OFFLINE.
 * NODEs poll their PRIMARY every 30s (sync.DefaultNodeSyncInterval) and the
 * primary throttles heartbeat writes to 15s, so 90s is three poll periods.
 */
export const NODE_OFFLINE_THRESHOLD_MS = 90_000;

const PRIMARY_ROLES = new Set<TopologyRole>(["primary", "primary-with-nodes"]);

/** The Nodes view is only meaningful (and only served) on a primary. */
export function canListNodes(role?: TopologyRole) {
  return role !== undefined && PRIMARY_ROLES.has(role);
}

export type NodeLiveness = { lastSeen: string; offline: boolean };

/** Derives the "last seen" text and the offline flag against the server clock. */
export function describeLiveness(node: TopologyNode, serverTime: string): NodeLiveness {
  const now = Date.parse(serverTime);
  const seen = node.lastSeenAt ? Date.parse(node.lastSeenAt) : Number.NaN;
  const linked = node.status === "linked";
  if (Number.isNaN(seen)) return { lastSeen: "never", offline: linked };
  if (Number.isNaN(now)) return { lastSeen: "—", offline: false };
  const age = Math.max(0, now - seen);
  return { lastSeen: formatAge(age), offline: linked && age > NODE_OFFLINE_THRESHOLD_MS };
}

function formatAge(ms: number) {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

export function nodesError(caught: unknown, action: "load" | "unlink") {
  if (caught instanceof TopologyApiError) {
    switch (caught.kind) {
      case "forbidden":
        return action === "unlink"
          ? "Owner access on a writable PRIMARY is required to unlink a node."
          : "Owner access is required to list nodes.";
      case "not-found":
        return "That node no longer exists. The list was refreshed.";
      case "not-primary":
        return "Nodes can only be managed from a PRIMARY.";
      case "unavailable":
        return "The nodes service is temporarily unavailable. Try again shortly.";
      case "network":
        return "The nodes service could not be reached. Check the connection and try again.";
      default:
        break;
    }
  }
  return action === "unlink" ? "The node could not be unlinked. Try again." : "The node list could not be loaded. Try again.";
}

function nodeLabel(node: TopologyNode) {
  return node.displayName || node.nodeId.slice(0, 8);
}

export function NodesView({ role }: { role?: TopologyRole }) {
  const enabled = canListNodes(role);
  const [data, setData] = useState<TopologyNodeList | undefined>();
  const [loading, setLoading] = useState(enabled);
  const [loadError, setLoadError] = useState("");
  const [actionError, setActionError] = useState("");
  const [confirmId, setConfirmId] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  const mounted = useRef(true);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await listTopologyNodes(signal);
      if (!mounted.current || signal?.aborted) return;
      setData(next);
      setLoadError("");
    } catch (caught) {
      if (mounted.current && !signal?.aborted) setLoadError(nodesError(caught, "load"));
    } finally {
      if (mounted.current && !signal?.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    if (!enabled) return undefined;
    const controller = new AbortController();
    setLoading(true);
    void load(controller.signal);
    return () => {
      mounted.current = false;
      controller.abort();
    };
  }, [enabled, load]);

  async function unlink(node: TopologyNode) {
    if (busy) return;
    setBusy(true);
    setActionError("");
    try {
      await unlinkTopologyNode(node.nodeId);
      if (mounted.current) setConfirmId(undefined);
    } catch (caught) {
      if (mounted.current) {
        setActionError(nodesError(caught, "unlink"));
        if (caught instanceof TopologyApiError && caught.kind === "not-found") setConfirmId(undefined);
      }
    }
    // Always re-read: after success it shows the new state, after 404 the node is gone.
    await load();
    if (mounted.current) setBusy(false);
  }

  if (!enabled) {
    return (
      <p className="mt-8 border-y border-dashed border-line px-1 py-5 text-sm text-faint" role="status" data-testid="nodes-unavailable">
        Nodes are listed only on a PRIMARY.
      </p>
    );
  }

  return (
    <section id="nodes" className="mt-8 space-y-4 border-t border-line/80 pt-6" aria-labelledby="nodes-title" data-testid="nodes-view">
      <header className="flex flex-wrap items-end justify-between gap-4 border-b border-line/80 pb-4">
        <div>
          <p className="pc-eyebrow pc-eyebrow-signal">cluster</p>
          <h2 id="nodes-title" className="pc-title mt-2 text-2xl text-mist">Nodes</h2>
        </div>
        <button type="button" className="pc-btn-ghost" data-testid="nodes-refresh" disabled={loading || busy} onClick={() => { setLoading(true); void load(); }}>
          Refresh
        </button>
      </header>
      {loadError ? <p className="pc-toast-err" role="alert" data-testid="nodes-load-error">{loadError}</p> : null}
      {actionError ? <p className="pc-toast-err" role="alert" data-testid="nodes-action-error">{actionError}</p> : null}
      {loading && !data ? <p className="text-sm text-faint" role="status">Loading nodes…</p> : null}
      {data && data.nodes.length === 0 ? (
        <p className="border-y border-dashed border-line px-1 py-5 text-sm leading-6 text-faint" data-testid="nodes-empty">
          No nodes are linked to this PRIMARY yet. Create an enrollment token in the enrollment token panel on the continuity page, then use it on the NODE installation to link it.
        </p>
      ) : null}
      {data && data.nodes.length > 0 ? (
        <ul className="border-y border-line/80" aria-label="Enrolled nodes">
          {data.nodes.map((node) => (
            <NodeRow
              key={node.nodeId}
              node={node}
              serverTime={data.serverTime}
              confirming={confirmId === node.nodeId}
              busy={busy}
              onAsk={() => { setActionError(""); setConfirmId(node.nodeId); }}
              onCancel={() => setConfirmId(undefined)}
              onConfirm={() => void unlink(node)}
            />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function NodeRow(props: {
  node: TopologyNode;
  serverTime: string;
  confirming: boolean;
  busy: boolean;
  onAsk: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { node } = props;
  const liveness = describeLiveness(node, props.serverTime);
  const name = nodeLabel(node);
  const unlinkable = node.status === "linked";
  return (
    <li className="space-y-3 border-b border-line/80 px-1 py-4 last:border-b-0" data-testid={`node-row-${node.nodeId}`}>
      <div className="grid gap-3 sm:grid-cols-[minmax(0,1.2fr)_minmax(0,1.2fr)_auto_auto] sm:items-center">
        <div className="min-w-0">
          <p className="truncate font-mono text-sm text-mist" data-testid="node-name">{name}</p>
          <p className="mt-1 truncate font-mono text-[11px] text-faint">{node.nodeId}</p>
        </div>
        <p className="font-mono text-xs text-mute" data-testid="node-ips">
          <span>IPv4 {node.ingressIPv4 || "—"}</span>
          <span className="ml-3">IPv6 {node.ingressIPv6 || "—"}</span>
        </p>
        <p className="font-mono text-xs text-faint" data-testid="node-last-seen">
          last seen {liveness.lastSeen}
        </p>
        <p className="flex flex-wrap gap-2">
          <span className="pc-eyebrow" data-testid="node-status">{node.status}</span>
          {liveness.offline ? <span className="pc-eyebrow text-danger" data-testid="node-offline">offline</span> : null}
        </p>
      </div>
      {unlinkable && !props.confirming ? (
        <button type="button" className="pc-btn-ghost" data-testid="node-unlink-open" aria-label={`Unlink ${name}`} disabled={props.busy} onClick={props.onAsk}>
          Unlink
        </button>
      ) : null}
      {props.confirming ? (
        <div className="space-y-3 border-2 border-signal/60 bg-signal/5 p-4" role="group" aria-label={`Confirm unlinking ${name}`} data-testid="node-unlink-confirm">
          <p className="text-sm leading-6 text-mute">
            Unlinking {name} revokes its credential. It keeps serving its last configuration, will be told on its next sync that it was unlinked, and stops receiving updates until it is linked again.
          </p>
          <div className="flex flex-wrap gap-2">
            <button type="button" className="pc-btn" data-testid="node-unlink-confirm-button" disabled={props.busy} onClick={props.onConfirm}>
              {props.busy ? "Unlinking…" : "Confirm unlink"}
            </button>
            <button type="button" className="pc-btn-ghost" data-testid="node-unlink-cancel" disabled={props.busy} onClick={props.onCancel}>
              Cancel
            </button>
          </div>
        </div>
      ) : null}
    </li>
  );
}
