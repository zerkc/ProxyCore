"use client";

import { FormEvent, useEffect, useState } from "react";
import type { StreamRoute } from "../types";

export function StreamDialog(props: {
  open: boolean;
  initial?: StreamRoute;
  onClose: () => void;
  onSubmit: (payload: Record<string, unknown>) => Promise<boolean>;
}) {
  const editing = !!props.initial;
  const [protocol, setProtocol] = useState<"tcp" | "udp">("tcp");
  const [listenAddress, setListenAddress] = useState("0.0.0.0");
  const [listenPort, setListenPort] = useState(9000);
  const [upstreamIp, setUpstreamIp] = useState("192.168.1.20");
  const [upstreamPort, setUpstreamPort] = useState(9000);
  const [enabled, setEnabled] = useState(true);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!props.open) return;
    setProtocol(props.initial?.protocol ?? "tcp");
    setListenAddress(props.initial?.listenAddress ?? "0.0.0.0");
    setListenPort(props.initial?.listenPort ?? 9000);
    setUpstreamIp(props.initial?.upstream.ip ?? "192.168.1.20");
    setUpstreamPort(props.initial?.upstream.port ?? 9000);
    setEnabled(props.initial?.enabled ?? true);
    setSubmitting(false);
  }, [props.open, props.initial]);

  if (!props.open) return null;

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    try {
      await props.onSubmit({
        enabled,
        protocol,
        listenAddress,
        listenPort,
        upstream: {
          ip: upstreamIp,
          port: upstreamPort,
          protocol,
        },
      });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center bg-bay/85 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="stream-dialog-title"
    >
      <form
        className="pc-panel w-full max-w-lg p-6 shadow-2xl shadow-black/40"
        onSubmit={handleSubmit}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">
              {editing ? "Edit stream" : "New stream"}
            </p>
            <h2
              id="stream-dialog-title"
              className="pc-title mt-2 text-2xl text-mist"
            >
              {editing ? "Update port forward" : "Forward a port"}
            </h2>
            <p className="mt-2 text-sm leading-6 text-mute">
              Expose a TCP or UDP listener on ProxyCore and send it to a literal
              upstream IP and port.
            </p>
          </div>
          <button
            type="button"
            className="text-sm text-mute transition hover:text-mist"
            onClick={props.onClose}
          >
            Close
          </button>
        </div>

        <div className="mt-6 grid gap-4 sm:grid-cols-2">
          <label className="pc-label">
            Protocol
            <select
              value={protocol}
              onChange={(event) =>
                setProtocol(event.target.value === "udp" ? "udp" : "tcp")
              }
              className="pc-input"
            >
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
            </select>
          </label>
          <label className="pc-label">
            Listen address
            <input
              value={listenAddress}
              onChange={(event) => setListenAddress(event.target.value)}
              className="pc-input"
              placeholder="0.0.0.0"
              required
            />
          </label>
          <label className="pc-label">
            Listen port
            <input
              type="number"
              min={1}
              max={65535}
              value={listenPort}
              onChange={(event) => setListenPort(Number(event.target.value))}
              className="pc-input"
              required
            />
          </label>
          <label className="pc-label">
            Upstream IP
            <input
              value={upstreamIp}
              onChange={(event) => setUpstreamIp(event.target.value)}
              className="pc-input"
              placeholder="192.168.1.20"
              required
            />
          </label>
          <label className="pc-label">
            Upstream port
            <input
              type="number"
              min={1}
              max={65535}
              value={upstreamPort}
              onChange={(event) => setUpstreamPort(Number(event.target.value))}
              className="pc-input"
              required
            />
          </label>
          <label className="flex items-center gap-3 self-end pb-2 text-sm text-mist/90">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(event) => setEnabled(event.target.checked)}
              className="size-4 accent-signal"
            />
            Enabled
          </label>
        </div>

        <div className="mt-6 flex justify-end gap-3">
          <button type="button" className="pc-btn-ghost" onClick={props.onClose}>
            Cancel
          </button>
          <button type="submit" className="pc-btn" disabled={submitting}>
            {submitting
              ? "Saving…"
              : editing
                ? "Save changes"
                : "Add stream"}
          </button>
        </div>
      </form>
    </div>
  );
}
