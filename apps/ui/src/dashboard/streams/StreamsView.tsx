import { useState } from "react";
import type { StreamRoute } from "../types";
import { StreamDialog } from "./StreamDialog";

export function StreamsView(props: {
  streams: StreamRoute[];
  saveStream: (payload: Record<string, unknown>) => Promise<boolean>;
  updateStream: (
    streamId: string,
    payload: Record<string, unknown>,
  ) => Promise<boolean>;
  deleteStream: (streamId: string) => Promise<boolean>;
}) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<StreamRoute>();
  const [busyId, setBusyId] = useState<string>();

  async function submitDialog(payload: Record<string, unknown>) {
    const saved = editing
      ? await props.updateStream(editing.id, payload)
      : await props.saveStream(payload);
    if (saved) {
      setDialogOpen(false);
      setEditing(undefined);
    }
    return saved;
  }

  async function toggleEnabled(stream: StreamRoute) {
    setBusyId(stream.id);
    try {
      await props.updateStream(stream.id, {
        ...stream,
        enabled: !stream.enabled,
      });
    } finally {
      setBusyId(undefined);
    }
  }

  async function remove(stream: StreamRoute) {
    const confirmed = window.confirm(
      `Delete ${stream.protocol.toUpperCase()} ${stream.listenAddress}:${stream.listenPort}?`,
    );
    if (!confirmed) return;
    setBusyId(stream.id);
    try {
      await props.deleteStream(stream.id);
    } finally {
      setBusyId(undefined);
    }
  }

  return (
    <div className="mt-8 space-y-6">
      <header className="flex flex-col gap-5 border-b border-line/80 pb-5 md:flex-row md:items-end md:justify-between">
        <div className="min-w-0">
          <p className="pc-eyebrow">streams</p>
          <h2 className="pc-title mt-2 text-2xl text-mist">
            TCP / UDP port maps
          </h2>
          <p className="mt-3 font-mono text-sm text-link">
            {props.streams.length}{" "}
            {props.streams.length === 1 ? "stream" : "streams"}
          </p>
          <p className="mt-2 max-w-xl text-sm leading-6 text-mute">
            Listeners exposed by ProxyCore and forwarded to a literal upstream
            IP and port on your network.
          </p>
        </div>
        <button
          type="button"
          className="pc-btn shrink-0 self-start md:self-auto"
          onClick={() => {
            setEditing(undefined);
            setDialogOpen(true);
          }}
        >
          Add stream
        </button>
      </header>

      {props.streams.length ? (
        <div className="border-y border-line/80">
          {props.streams.map((stream) => {
            const busy = busyId === stream.id;
            return (
              <article
                key={stream.id}
                className={`grid gap-3 border-b border-line/80 px-1 py-3.5 transition-colors last:border-b-0 sm:grid-cols-[auto_minmax(0,1fr)_auto] sm:items-center ${
                  stream.enabled
                    ? "hover:bg-panel/40"
                    : "bg-raised/30 opacity-80"
                }`}
              >
                <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 sm:min-w-[132px]">
                  <span
                    className={`inline-flex items-center gap-2 font-mono text-[11px] uppercase tracking-[0.12em] ${
                      stream.enabled ? "text-ok" : "text-faint"
                    }`}
                  >
                    <span
                      className={`h-1.5 w-1.5 shrink-0 ${
                        stream.enabled ? "bg-ok" : "bg-faint"
                      }`}
                      aria-hidden
                    />
                    {stream.enabled ? "enabled" : "disabled"}
                  </span>
                  <span className="border-l border-line pl-3 font-mono text-[11px] uppercase tracking-[0.12em] text-link">
                    {stream.protocol}
                  </span>
                </div>
                <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 font-mono">
                  <span className="break-all text-sm text-mist">
                    {stream.listenAddress}:{stream.listenPort}
                  </span>
                  <span className="text-faint" aria-hidden>
                    →
                  </span>
                  <span className="break-all text-sm text-link">
                    {stream.upstream.ip}:{stream.upstream.port}
                  </span>
                </div>
                <div className="flex flex-wrap gap-2 sm:justify-end">
                  <button
                    type="button"
                    className="pc-btn-ghost !text-xs"
                    disabled={busy}
                    onClick={() => void toggleEnabled(stream)}
                  >
                    {stream.enabled ? "Disable" : "Enable"}
                  </button>
                  <button
                    type="button"
                    className="pc-btn-ghost !text-xs"
                    disabled={busy}
                    onClick={() => {
                      setEditing(stream);
                      setDialogOpen(true);
                    }}
                  >
                    Modify
                  </button>
                  <button
                    type="button"
                    className="rounded-none border border-danger/40 px-3 py-2 text-xs text-danger transition hover:bg-danger/10 disabled:opacity-50"
                    disabled={busy}
                    onClick={() => void remove(stream)}
                  >
                    Delete
                  </button>
                </div>
              </article>
            );
          })}
        </div>
      ) : (
        <p className="border-y border-dashed border-line py-5 text-sm text-faint">
          No TCP/UDP listeners yet. Add a stream to forward a port into your
          network.
        </p>
      )}

      <StreamDialog
        open={dialogOpen}
        initial={editing}
        onClose={() => {
          setDialogOpen(false);
          setEditing(undefined);
        }}
        onSubmit={submitDialog}
      />
    </div>
  );
}
