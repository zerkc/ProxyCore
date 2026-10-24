import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { api } from "../api";
import type { StatusPayload, UpdatePayload } from "./types";

type RuntimeStatus = Pick<
  StatusPayload,
  "version" | "updating" | "updateTarget"
>;

type VersionStatusProps = {
  update?: UpdatePayload;
  /** Polled status (every few seconds); drives the update dialog phases. */
  status?: RuntimeStatus;
  loading: boolean;
  error: string;
  onRetry: () => void;
  /** Reports whether an update is being applied (the API restarts mid-update). */
  onApplyingChange?: (applying: boolean) => void;
};

/**
 * Local UI state for the "Update" flow.
 *
 * idle        - nothing happening; show "Update to v…" button
 * confirming  - user clicked once; show Cancel + Confirm pair
 * applying    - update started (or resumed); phase follows the polled status
 * timeout     - applying exceeded APPLY_TIMEOUT_MS; user can keep waiting
 * done        - the running version changed (dialog stays open until the user
 *               refreshes)
 * failed      - the updater stopped but the version did not change
 */
type ApplyPhase =
  "idle" | "confirming" | "applying" | "timeout" | "done" | "failed";

/** Snapshot taken when an update starts, used to judge the outcome. */
type ApplyRun = {
  fromVersion?: string;
  startedAt: number;
  sawUpdating: boolean;
};

const APPLY_TIMEOUT_MS = 5 * 60 * 1_000;
const APPLY_REQUEST_TIMEOUT_MS = 5_000;
/** An idle updater this long after the POST, never seen running, is a failure. */
const FAILURE_GRACE_MS = 15_000;

function normalizeVersion(version?: string | null): string | undefined {
  const normalized = version?.trim().replace(/^v/, "");
  return normalized || undefined;
}

export function VersionStatus({
  update,
  status,
  loading,
  error,
  onRetry,
  onApplyingChange,
}: VersionStatusProps) {
  const [phase, setPhase] = useState<ApplyPhase>("idle");
  const [applyTargetVersion, setApplyTargetVersion] = useState<string>();
  const runRef = useRef<ApplyRun>({ startedAt: 0, sawUpdating: false });
  const latest = update?.latest;
  const latestVersion = normalizeVersion(latest?.version);
  const runningVersion =
    normalizeVersion(status?.version) ??
    normalizeVersion(update?.currentVersion);
  const targetVersion =
    normalizeVersion(applyTargetVersion) ??
    normalizeVersion(status?.updateTarget) ??
    latestVersion;

  function beginRun() {
    runRef.current = {
      fromVersion: runningVersion,
      startedAt: Date.now(),
      sawUpdating: status?.updating === true,
    };
  }

  // Resume an update that was already running before this page loaded. Status
  // alone is enough, so read-only users see the dialog too.
  useEffect(() => {
    if (phase !== "idle" || status?.updating !== true) return;
    const target = normalizeVersion(status.updateTarget) ?? latestVersion;
    if (!target) return;
    runRef.current = {
      fromVersion: runningVersion,
      startedAt: Date.now(),
      sawUpdating: true,
    };
    setApplyTargetVersion(target);
    setPhase("applying");
  }, [
    phase,
    status?.updating,
    status?.updateTarget,
    latestVersion,
    runningVersion,
  ]);

  // Derive the outcome from the polled status. An unknown updater state
  // (null) and missing status (failed requests while the API restarts) never
  // count as failure.
  useEffect(() => {
    if (phase !== "applying" && phase !== "timeout") return;
    if (!status) return;
    const run = runRef.current;
    if (status.updating === true) {
      run.sawUpdating = true;
      return;
    }
    const current = normalizeVersion(status.version);
    const changed =
      Boolean(current) &&
      (current === targetVersion ||
        (run.fromVersion !== undefined && current !== run.fromVersion));
    if (changed) {
      setPhase("done");
      return;
    }
    if (
      status.updating === false &&
      run.fromVersion !== undefined &&
      current === run.fromVersion &&
      (run.sawUpdating || Date.now() - run.startedAt >= FAILURE_GRACE_MS)
    ) {
      setPhase("failed");
    }
  }, [phase, status, targetVersion]);

  // Tell the dashboard an update is being applied so a 401 while the API
  // restarts does not bounce the user to the login page.
  const applying = phase === "applying" || phase === "timeout";
  useEffect(() => {
    onApplyingChange?.(applying);
    return () => onApplyingChange?.(false);
  }, [applying, onApplyingChange]);

  // Watchdog so the UI surfaces "taking too long" while status keeps saying
  // the updater is running (or stays unknown).
  useEffect(() => {
    if (phase !== "applying") return;
    const id = setTimeout(() => setPhase("timeout"), APPLY_TIMEOUT_MS);
    return () => clearTimeout(id);
  }, [phase]);

  async function startApply() {
    if (!targetVersion) return;
    beginRun();
    setPhase("applying");
    // Fire-and-forget POST. We bound the request so we don't hang on a slow
    // server death; even if this throws, the polled status carries on and
    // will eventually show the outcome.
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
      // Expected while the API is restarting; status polling continues.
    }
  }

  const isBlocking =
    phase === "applying" ||
    phase === "timeout" ||
    phase === "done" ||
    phase === "failed";

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

  const hasUpdate =
    update.status === "update_available" && Boolean(latestVersion);
  const statusMessage =
    update.status === "disabled"
      ? "Update checks off"
      : update.status === "stale"
        ? "Last check is being used"
        : update.status === "unavailable"
          ? "Could not check GitHub"
          : "Up to date";

  return (
    <>
      <section
        className="mt-6 border-t border-line/80 pt-4"
        aria-label="Version status"
        aria-live="polite"
      >
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="pc-eyebrow">Version</p>
            <p className="mt-2 font-mono text-sm text-link">
              v
              {normalizeVersion(update.currentVersion) ?? update.currentVersion}
            </p>
          </div>
          {phase === "idle" ? (
            <RetryButton loading={loading} onRetry={onRetry} />
          ) : null}
        </div>

        {hasUpdate && latestVersion && phase === "idle" ? (
          <UpdateAvailableCard
            version={latestVersion}
            onConfirm={() => {
              setApplyTargetVersion(latestVersion);
              setPhase("confirming");
            }}
          />
        ) : null}

        {hasUpdate && latestVersion && phase === "confirming" ? (
          <ConfirmCard
            version={latestVersion}
            onCancel={() => {
              setApplyTargetVersion(undefined);
              setPhase("idle");
            }}
            onConfirm={() => void startApply()}
          />
        ) : null}

        {!hasUpdate && phase === "idle" ? (
          <p className="mt-2 text-xs leading-5 text-mute">{statusMessage}</p>
        ) : null}

        {error && phase === "idle" ? (
          <p className="mt-2 text-xs text-mute">{error}</p>
        ) : null}
      </section>

      {/* Blocking update dialog — rendered under document.body via portal */}
      {isBlocking && targetVersion
        ? createPortal(
            <UpdateBlockingDialog
              phase={phase}
              targetVersion={targetVersion}
              runningVersion={runningVersion}
              onRefresh={() => window.location.reload()}
              onRetryContinue={() => setPhase("applying")}
              onDismiss={() => {
                setApplyTargetVersion(undefined);
                setPhase("idle");
                onRetry();
              }}
            />,
            document.body,
          )
        : null}
    </>
  );
}

// ─── Blocking update dialog ─────────────────────────────────────────────────

type UpdateBlockingDialogProps = {
  phase: "applying" | "timeout" | "done" | "failed";
  targetVersion: string;
  runningVersion?: string;
  onRefresh: () => void;
  onRetryContinue: () => void;
  onDismiss: () => void;
};

function UpdateBlockingDialog({
  phase,
  targetVersion,
  runningVersion,
  onRefresh,
  onRetryContinue,
  onDismiss,
}: UpdateBlockingDialogProps) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const previousFocusRef = useRef<Element | null>(null);

  // ── Mount: capture previous focus, move into dialog ──────────────────────
  useEffect(() => {
    previousFocusRef.current = document.activeElement;

    const focusable = dialogRef.current?.querySelector<HTMLElement>(
      'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
    );
    // Small delay so the DOM is painted before moving focus.
    const id = setTimeout(() => {
      if (focusable) {
        focusable.focus();
      } else {
        dialogRef.current?.focus();
      }
    }, 10);

    return () => clearTimeout(id);
  }, []);

  // ── Cleanup: restore focus, unlock body scroll ─────────────────────────
  useEffect(() => {
    const scrollY = window.scrollY;
    const previousBodyStyle = {
      position: document.body.style.position,
      top: document.body.style.top,
      width: document.body.style.width,
      overflow: document.body.style.overflow,
    };
    const root = document.getElementById("root");
    const wasInert = root?.hasAttribute("inert") ?? false;
    const previousAriaHidden = root?.getAttribute("aria-hidden") ?? null;

    document.body.style.position = "fixed";
    document.body.style.top = `-${scrollY}px`;
    document.body.style.width = "100%";
    document.body.style.overflow = "hidden";
    if (root) {
      root.setAttribute("inert", "");
      root.setAttribute("aria-hidden", "true");
    }

    return () => {
      document.body.style.position = previousBodyStyle.position;
      document.body.style.top = previousBodyStyle.top;
      document.body.style.width = previousBodyStyle.width;
      document.body.style.overflow = previousBodyStyle.overflow;
      if (root) {
        if (wasInert) root.setAttribute("inert", "");
        else root.removeAttribute("inert");
        if (previousAriaHidden === null) root.removeAttribute("aria-hidden");
        else root.setAttribute("aria-hidden", previousAriaHidden);
      }
      window.scrollTo(0, scrollY);

      // Restore focus to wherever it was before the dialog opened.
      if (
        previousFocusRef.current instanceof HTMLElement &&
        previousFocusRef.current.isConnected
      ) {
        previousFocusRef.current.focus();
      }
    };
  }, []);

  // ── Reduced-motion: skip entrance animation ─────────────────────────────
  const prefersReduced = usePrefersReducedMotion();

  const title =
    phase === "done"
      ? "Update complete"
      : phase === "failed"
        ? "Update failed"
        : phase === "timeout"
          ? "Still updating"
          : "Installing update";

  const headingId = "update-dialog-title";
  const descId = "update-dialog-desc";

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={headingId}
      aria-describedby={descId}
      ref={dialogRef}
      tabIndex={-1}
      className={[
        "fixed inset-0 z-[9999] flex flex-col items-center justify-center p-4",
        "bg-bay/95 backdrop-blur-sm",
        prefersReduced ? "" : "animate-dialog-in",
      ]
        .filter(Boolean)
        .join(" ")}
      onKeyDown={(e) => {
        // Trap focus: Tab cycles within the dialog only.
        if (e.key === "Tab") {
          const focusable = dialogRef.current?.querySelectorAll<HTMLElement>(
            'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
          );
          if (!focusable || focusable.length === 0) return;
          const first = focusable[0];
          const last = focusable[focusable.length - 1];
          if (e.shiftKey) {
            if (document.activeElement === first) {
              e.preventDefault();
              last.focus();
            }
          } else {
            if (document.activeElement === last) {
              e.preventDefault();
              first.focus();
            }
          }
        }
      }}
    >
      <div
        className={[
          "flex w-full max-w-sm flex-col items-center gap-5 rounded-none",
          "border border-signal/40 bg-raised p-8",
          "text-center",
          prefersReduced ? "" : "animate-card-in",
        ]
          .filter(Boolean)
          .join(" ")}
      >
        {/* Status indicator */}
        <div className="relative flex h-12 w-12 items-center justify-center">
          {phase === "applying" ? (
            <span
              className="pc-spinner pc-update-spinner absolute inset-0 m-auto"
              aria-hidden
            />
          ) : phase === "failed" ? (
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="absolute inset-0 m-auto h-10 w-10 text-signal"
              aria-hidden
            >
              <circle cx="12" cy="12" r="10" />
              <line x1="15" y1="9" x2="9" y2="15" />
              <line x1="9" y1="9" x2="15" y2="15" />
            </svg>
          ) : phase === "timeout" ? (
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="absolute inset-0 m-auto h-10 w-10 text-signal"
              aria-hidden
            >
              <circle cx="12" cy="12" r="10" />
              <polyline points="12 6 12 12 16 14" />
            </svg>
          ) : (
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="absolute inset-0 m-auto h-10 w-10 text-ok"
              aria-hidden
            >
              <polyline points="20 6 9 17 4 12" />
            </svg>
          )}
        </div>

        {/* Heading */}
        <div>
          <h2
            id={headingId}
            className={[
              "pc-title text-xl",
              phase === "done"
                ? "text-ok"
                : phase === "failed" || phase === "timeout"
                  ? "text-signal"
                  : "text-mist",
            ].join(" ")}
          >
            {title}
          </h2>
          <p className="mt-1 font-mono text-sm text-link">v{targetVersion}</p>
        </div>

        {/* Description */}
        <p id={descId} className="text-sm leading-6 text-mute">
          {phase === "done"
            ? "ProxyCore has been updated. Refresh the page to load the new version."
            : phase === "failed"
              ? `The update stopped but ProxyCore is still running v${runningVersion ?? "unknown"}, so the new version was not installed. Check the updater logs, then try again.`
              : phase === "timeout"
                ? "The update is taking longer than expected but is still in progress. Do not close or reload this tab — editing is locked until it is safe to continue."
                : "ProxyCore is being updated. Do not close or reload this tab — editing is locked until it is safe to continue."}
        </p>

        {/* Actions */}
        <div className="mt-2 flex w-full flex-col gap-2">
          {phase === "done" ? (
            <button
              type="button"
              className="pc-btn w-full"
              onClick={onRefresh}
              autoFocus
            >
              Refresh page
            </button>
          ) : phase === "failed" ? (
            <button
              type="button"
              className="pc-btn w-full"
              onClick={onDismiss}
              autoFocus
            >
              Close
            </button>
          ) : phase === "timeout" ? (
            <>
              <button
                type="button"
                className="pc-btn w-full"
                onClick={onRetryContinue}
                autoFocus
              >
                Keep waiting
              </button>
              <p className="text-center text-xs text-mute">
                Editing is locked while ProxyCore verifies the new version.
              </p>
            </>
          ) : (
            <p className="text-center text-xs text-mute">
              Holding… editing stays locked until the new version comes back
              online.
            </p>
          )}
        </div>

        {/* Aria-live region for screen readers */}
        <div className="sr-only" aria-live="polite" aria-atomic="true">
          {phase === "applying"
            ? `Installing update to version ${targetVersion}. Do not reload the page.`
            : phase === "failed"
              ? `Update to version ${targetVersion} failed. The running version did not change.`
              : phase === "timeout"
                ? `Update to version ${targetVersion} is taking longer than expected but is still in progress.`
                : `Update to version ${targetVersion} is complete. Refresh the page to load the new version.`}
        </div>
      </div>
    </div>
  );
}

// ─── Idle/confirming cards (remain inline) ────────────────────────────────

function UpdateAvailableCard({
  version,
  onConfirm,
}: {
  version: string;
  onConfirm: () => void;
}) {
  return (
    <div className="mt-3 rounded-none border border-signal/30 bg-signal/10 p-3">
      <p className="pc-eyebrow pc-eyebrow-signal">Update available</p>
      <p className="mt-2 font-mono text-sm text-mist">v{version}</p>
      <button type="button" className="pc-btn mt-3 w-full" onClick={onConfirm}>
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
    <div className="mt-3 rounded-none border border-signal/40 bg-signal/10 p-3">
      <p className="pc-eyebrow pc-eyebrow-signal">Confirm update</p>
      <p className="mt-2 text-xs leading-5 text-mist">
        This will restart ProxyCore to install v{version}. An update may
        interrupt unsaved edits — save your changes before confirming.
      </p>
      <div className="mt-3 flex gap-2">
        <button
          type="button"
          className="pc-btn-ghost flex-1"
          onClick={onCancel}
        >
          Cancel
        </button>
        <button type="button" className="pc-btn flex-1" onClick={onConfirm}>
          Yes, update now
        </button>
      </div>
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
      {loading ? "Checking…" : "Check for updates"}
    </button>
  );
}

function usePrefersReducedMotion(): boolean {
  const [prefers, setPrefers] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia("(prefers-reduced-motion: reduce)");
    setPrefers(mq.matches);
    const handler = (e: MediaQueryListEvent) => setPrefers(e.matches);
    mq.addEventListener("change", handler);
    return () => mq.removeEventListener("change", handler);
  }, []);
  return prefers;
}
