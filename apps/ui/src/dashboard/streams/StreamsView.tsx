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
      <section className="pc-panel p-6 md:p-8">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <p className="pc-eyebrow">Configured streams</p>
            <h2 className="pc-title mt-2 text-2xl text-mist">
              TCP / UDP port maps
            </h2>
            <p className="mt-2 max-w-xl text-sm leading-6 text-mute">
              Listeners exposed by ProxyCore and forwarded to a literal upstream
              IP and port on your network.
            </p>
          </div>
          <button
            type="button"
            className="pc-btn"
            onClick={() => {
              setEditing(undefined);
              setDialogOpen(true);
            }}
          >
            Add stream
          </button>
        </div>
        <p className="mt-4 font-mono text-sm text-link">
          {props.streams.length}{" "}
          {props.streams.length === 1 ? "stream" : "streams"}
        </p>
      </section>

      <section className="space-y-2">
        {props.streams.length ? (
          props.streams.map((stream) => {
            const busy = busyId === stream.id;
            return (
              <article
                key={stream.id}
                className={`rounded-xl border px-4 py-4 transition sm:px-5 ${
                  stream.enabled
                    ? "border-line/80 bg-bay/50"
                    : "border-line/50 bg-raised/30 opacity-80"
                }`}
              >
                <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <span
                        className={`rounded-md px-2 py-0.5 font-mono text-[11px] uppercase tracking-[0.12em] ${
                          stream.enabled
                            ? "bg-ok/15 text-ok"
                            : "bg-faint/20 text-faint"
                        }`}
                      >
                        {stream.enabled ? "enabled" : "disabled"}
                      </span>
                      <span className="rounded-md bg-link/10 px-2 py-0.5 font-mono text-[11px] uppercase tracking-[0.12em] text-link">
                        {stream.protocol}
                      </span>
                    </div>
                    <p className="mt-2 font-mono text-base text-mist">
                      {stream.listenAddress}:{stream.listenPort}
                    </p>
                    <p className="mt-1 font-mono text-xs text-faint">
                      → {stream.upstream.ip}:{stream.upstream.port}
                    </p>
                  </div>
                  <div className="flex flex-wrap gap-2">
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
                      className="rounded-xl border border-danger/40 px-3 py-2 text-xs text-danger transition hover:bg-danger/10 disabled:opacity-50"
                      disabled={busy}
                      onClick={() => void remove(stream)}
                    >
                      Delete
                    </button>
                  </div>
                </div>
              </article>
            );
          })
        ) : (
          <p className="rounded-xl border border-dashed border-line p-6 text-sm text-faint">
            No TCP/UDP listeners yet. Add a stream to forward a port into your
            network.
          </p>
        )}
      </section>

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
