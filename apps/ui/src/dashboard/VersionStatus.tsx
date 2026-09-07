import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import type { UpdatePayload } from "./types";

type VersionStatusProps = {
  update?: UpdatePayload;
  loading: boolean;
  error: string;
  onRetry: () => void;
};

/**
 * Local UI state for the "Update" flow.
 *
 * idle        - nothing happening; show "Update to v…" button
 * confirming  - user clicked once; show Cancel + Confirm pair
 * applying    - POST /api/updates/apply fired; polling every 3s
 * done        - update.currentVersion === latest.version
 * timeout     - apply exceeded APPLY_TIMEOUT_MS; polling continues
 */
type ApplyPhase = "idle" | "confirming" | "applying" | "done" | "timeout";

const POLL_INTERVAL_MS = 5_000;
const POLL_MAX_INTERVAL_MS = 30_000;
const APPLY_TIMEOUT_MS = 5 * 60 * 1_000;
const APPLY_REQUEST_TIMEOUT_MS = 5_000;

export function VersionStatus({
  update,
  loading,
  error,
  onRetry,
}: VersionStatusProps) {
  const [phase, setPhase] = useState<ApplyPhase>("idle");
  const latest = update?.latest;
  const targetVersion = latest?.version;
  const consecutiveErrorsRef = useRef(0);

  // Reset to idle if a newer update appears after a previous "done" state
  // (e.g. user just installed 0.1.5 and 0.1.6 was released shortly after).
  useEffect(() => {
    if (
      phase === "done" &&
      targetVersion &&
      update?.currentVersion !== targetVersion
    ) {
      setPhase("idle");
    }
  }, [phase, targetVersion, update?.currentVersion]);

  // Detect success: poll handler will trigger this through onRetry/update.
  useEffect(() => {
    if (phase !== "applying" && phase !== "timeout") return;
    if (!update || !targetVersion) return;
    if (update.currentVersion === targetVersion) {
      setPhase("done");
    }
  }, [phase, update, targetVersion]);

  // Poll the version endpoint while applying. Failures (server restarting) are
  // expected and ignored — the next tick will retry.
  useEffect(() => {
    if (phase !== "applying") return;
    consecutiveErrorsRef.current = 0;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function tick() {
      let success = false;
      try {
        await onRetry();
        success = true;
      } catch {
        // Server is probably restarting; keep polling with backoff.
      }
      if (cancelled) return;

      let nextInterval: number;
      if (success) {
        consecutiveErrorsRef.current = 0;
        nextInterval = POLL_INTERVAL_MS;
      } else {
        consecutiveErrorsRef.current += 1;
        nextInterval = Math.min(
          POLL_INTERVAL_MS * Math.pow(2, consecutiveErrorsRef.current - 1),
          POLL_MAX_INTERVAL_MS,
        );
      }
      timer = setTimeout(tick, nextInterval);
    }

    // Wait one interval before the first check: the POST just fired, the API
    // is about to die, and the initial onRetry is redundant noise.
    timer = setTimeout(tick, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [phase, onRetry]);

  // Watchdog so the UI surfaces "taking too long" without abandoning the poll.
  useEffect(() => {
    if (phase !== "applying") return;
    const id = setTimeout(() => setPhase("timeout"), APPLY_TIMEOUT_MS);
    return () => clearTimeout(id);
  }, [phase]);

  async function startApply() {
    if (!targetVersion) return;
    setPhase("applying");
    // Fire-and-forget POST. We bound the request so we don't hang on a slow
    // server death; even if this throws, the polling loop above carries on
    // and will eventually observe the new version.
    try {
      const controller = new AbortController();
      const timer = setTimeout(
        () => controller.abort(),
        APPLY_REQUEST_TIMEOUT_MS,
      );
      try {
        await api("/api/updates/apply", {
          method: "POST",
          body: JSON.stringify({ targetVersion }),
          signal: controller.signal,
        });
      } finally {
        clearTimeout(timer);
      }
    } catch {
      // Expected while the API is restarting; polling continues.
    }
  }

  if (!update) {
    return (
      <section
        className="mt-6 border-t border-line/80 pt-4"
        aria-label="Version status"
      >
        <div className="flex items-center justify-between gap-3">
          <div>
            <p className="pc-eyebrow">Version</p>
            <p className="mt-2 text-xs text-mute">
              {loading ? "Checking for updates…" : error}
            </p>
          </div>
          {!loading && error ? (
            <RetryButton loading={loading} onRetry={onRetry} />
          ) : null}
        </div>
      </section>
    );
  }

  const hasUpdate = update.status === "update_available" && Boolean(latest);
  const canRetry =
    Boolean(error) ||
    update.status === "stale" ||
    update.status === "unavailable";
  const statusMessage =
    update.status === "disabled"
      ? "Update checks off"
      : update.status === "stale"
        ? "Last check is being used"
        : update.status === "unavailable"
          ? "Could not check GitHub"
          : "Up to date";

  return (
    <section
      className="mt-6 border-t border-line/80 pt-4"
      aria-label="Version status"
      aria-live="polite"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="pc-eyebrow">Version</p>
          <p className="mt-2 font-mono text-sm text-link">
            v{update.currentVersion}
          </p>
        </div>
        {canRetry && phase === "idle" ? (
          <RetryButton loading={loading} onRetry={onRetry} />
        ) : null}
      </div>

      {hasUpdate && latest && phase === "idle" ? (
        <UpdateAvailableCard
          version={latest.version}
          onConfirm={() => setPhase("confirming")}
        />
      ) : null}

      {hasUpdate && latest && phase === "confirming" ? (
        <ConfirmCard
          version={latest.version}
          onCancel={() => setPhase("idle")}
          onConfirm={() => void startApply()}
        />
      ) : null}

      {(phase === "applying" || phase === "timeout") && targetVersion ? (
        <ApplyProgressCard
          targetVersion={targetVersion}
          timedOut={phase === "timeout"}
        />
      ) : null}

      {phase === "done" && targetVersion ? (
        <DoneCard version={targetVersion} />
      ) : null}

      {!hasUpdate && phase === "idle" ? (
        <p className="mt-2 text-xs leading-5 text-mute">{statusMessage}</p>
      ) : null}

      {error && phase === "idle" ? (
        <p className="mt-2 text-xs text-mute">{error}</p>
      ) : null}
    </section>
  );
}

function UpdateAvailableCard({
  version,
  onConfirm,
}: {
  version: string;
  onConfirm: () => void;
}) {
  return (
    <div className="mt-3 rounded-xl border border-signal/30 bg-signal/10 p-3">
      <p className="pc-eyebrow pc-eyebrow-signal">Update available</p>
      <p className="mt-2 font-mono text-sm text-mist">v{version}</p>
      <button
        type="button"
        className="pc-btn mt-3 w-full"
        onClick={onConfirm}
      >
        Update to v{version}
      </button>
    </div>
  );
}

function ConfirmCard({
  version,
  onCancel,
  onConfirm,
}: {
  version: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div className="mt-3 rounded-xl border border-signal/40 bg-signal/10 p-3">
      <p className="pc-eyebrow pc-eyebrow-signal">Confirm update</p>
      <p className="mt-2 text-xs leading-5 text-mist">
        This will restart ProxyCore to install v{version}. The UI will go
        offline for a moment while services come back.
      </p>
      <div className="mt-3 flex gap-2">
        <button
          type="button"
          className="pc-btn-ghost flex-1"
          onClick={onCancel}
        >
          Cancel
        </button>
        <button
          type="button"
          className="pc-btn flex-1"
          onClick={onConfirm}
        >
          Yes, update now
        </button>
      </div>
    </div>
  );
}

function ApplyProgressCard({
  targetVersion,
  timedOut,
}: {
  targetVersion: string;
  timedOut: boolean;
}) {
  return (
    <div
      className="mt-3 rounded-xl border border-signal/40 bg-signal/10 p-3"
      role="status"
      aria-live="polite"
    >
      <div className="flex items-center gap-2">
        {!timedOut ? <span className="pc-spinner" aria-hidden /> : null}
        <p className="pc-eyebrow pc-eyebrow-signal">
          {timedOut ? "Still updating" : "Installing update"}
        </p>
      </div>
      <p className="mt-2 font-mono text-sm text-mist">v{targetVersion}</p>
      <p className="mt-2 text-xs leading-5 text-mute">
        {timedOut
          ? "Taking longer than expected. The page keeps polling the new version in the background."
          : "Holding the connection… the page will refresh itself when the new version comes back online."}
      </p>
    </div>
  );
}

function DoneCard({ version }: { version: string }) {
  return (
    <div className="mt-3 rounded-xl border border-ok/30 bg-ok/10 p-3">
      <p className="pc-eyebrow" style={{ color: "var(--ok)" }}>
        Updated
      </p>
      <p className="mt-2 font-mono text-sm text-mist">v{version} is now live</p>
      <button
        type="button"
        className="pc-btn-ghost mt-3 w-full"
        onClick={() => window.location.reload()}
      >
        Refresh page
      </button>
    </div>
  );
}

function RetryButton({
  loading,
  onRetry,
}: {
  loading: boolean;
  onRetry: () => void;
}) {
  return (
    <button
      type="button"
      className="text-right text-[11px] text-mute underline decoration-line underline-offset-4 transition hover:text-mist disabled:cursor-wait disabled:opacity-60"
      onClick={onRetry}
      disabled={loading}
    >
      {loading ? "Checking…" : "Check again"}
    </button>
  );
}