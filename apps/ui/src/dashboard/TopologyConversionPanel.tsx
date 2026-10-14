import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, demoteTopology, promoteTopology, TopologyApiError } from "../api";
import type { TopologyIdentity } from "./types";

type Direction = "promote" | "demote";

export function conversionDirection(identity?: TopologyIdentity): Direction | undefined {
  if (!identity || identity.stalePrimary) return undefined;
  if (identity.role === "node") return "promote";
  if (identity.role === "primary" || identity.role === "primary-with-nodes") return "demote";
  return undefined;
}

export function TopologyConversionPanel({
  identity,
  onConverted,
}: {
  identity?: TopologyIdentity;
  onConverted: () => Promise<void> | void;
}) {
  const navigate = useNavigate();
  const request = useRef<AbortController | undefined>(undefined);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const direction = conversionDirection(identity);

  useEffect(() => {
    return () => request.current?.abort();
  }, []);

  useEffect(() => {
    setConfirming(false);
    setError("");
  }, [direction]);

  if (!direction || !identity) return null;
  const promote = direction === "promote";

  async function submit() {
    if (busy || !identity) return;
    const controller = new AbortController();
    request.current?.abort();
    request.current = controller;
    setBusy(true);
    setError("");
    try {
      const generation = identity.leadershipGeneration;
      if (promote) await promoteTopology(generation, controller.signal);
      else await demoteTopology(generation, controller.signal);
      if (controller.signal.aborted) return;
      setConfirming(false);
      await onConverted();
    } catch (caught) {
      if (controller.signal.aborted) return;
      if (caught instanceof ApiError && caught.status === 401) {
        navigate("/login");
        return;
      }
      setError(conversionError(caught, direction as Direction));
    } finally {
      if (request.current === controller) request.current = undefined;
      if (!controller.signal.aborted) setBusy(false);
    }
  }

  return (
    <section
      className="mt-8 space-y-5 border-t border-line/80 pt-6"
      aria-labelledby="topology-conversion-title"
      data-testid="topology-conversion"
    >
      <header className="border-b border-line/80 pb-5">
        <p className="pc-eyebrow pc-eyebrow-signal">owner authority</p>
        <h2 id="topology-conversion-title" className="pc-title mt-2 text-2xl text-mist">
          {promote ? "Promote to PRIMARY" : "Convert to NODE"}
        </h2>
        <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
          {promote
            ? "Make this installation the PRIMARY for planned switchover or failover."
            : "Turn this PRIMARY into a read-only NODE and enroll it against another PRIMARY."}
        </p>
      </header>
      {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
      {confirming ? (
        <div className="space-y-4 border-2 border-signal/60 bg-signal/5 p-4" data-testid="topology-confirmation">
          {promote ? (
            <ul className="list-disc space-y-2 pl-5 text-sm leading-6 text-mute">
              <li>Risk of split-brain: if the previous PRIMARY is still running it must not accept writes. Stop it or convert it to NODE.</li>
              <li>Nodes of the previous PRIMARY are not repointed and must be re-enrolled manually against this installation.</li>
              <li>The cluster key is rotated and old node credentials are revoked.</li>
            </ul>
          ) : (
            <ul className="list-disc space-y-2 pl-5 text-sm leading-6 text-mute">
              <li>Nodes enrolled with this installation lose their credentials and can no longer sync.</li>
              <li>This installation becomes a read-only NODE until it is enrolled against a PRIMARY.</li>
              <li>You will be asked for an enrollment token and PRIMARY URL next.</li>
            </ul>
          )}
          <div className="flex flex-wrap gap-2">
            <button type="button" className="pc-btn" data-testid="topology-confirm" disabled={busy} onClick={() => void submit()}>
              {busy ? "Converting…" : promote ? "Confirm promotion to PRIMARY" : "Confirm conversion to NODE"}
            </button>
            <button type="button" className="pc-btn-ghost" data-testid="topology-cancel" disabled={busy} onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </div>
        </div>
      ) : (
        <button
          type="button"
          className="pc-btn"
          data-testid={promote ? "topology-promote-open" : "topology-demote-open"}
          onClick={() => { setError(""); setConfirming(true); }}
        >
          {promote ? "Promote to PRIMARY" : "Convert to NODE"}
        </button>
      )}
    </section>
  );
}

function conversionError(caught: unknown, direction: Direction) {
  const action = direction === "promote" ? "promotion" : "conversion";
  if (caught instanceof TopologyApiError) {
    switch (caught.kind) {
      case "generation-mismatch":
        return "The leadership generation changed. Refresh the dashboard and try again.";
      case "invalid-transition":
        return `This ${action} is not allowed from the current role.`;
      case "forbidden":
        return `Owner access is required for this ${action}.`;
      case "unavailable":
        return `The ${action} service is temporarily unavailable. Try again shortly.`;
      case "bad-request":
        return `The ${action} request was rejected. Confirm the action and try again.`;
      case "network":
        return "The topology service could not be reached. Check the connection and try again.";
      default:
        break;
    }
  }
  return `The ${action} could not be completed. Try again.`;
}
