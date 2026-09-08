import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
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
 * applying    - POST /api/updates/apply fired; polling every 5s
 * timeout     - apply exceeded APPLY_TIMEOUT_MS; user can resume polling
 * done        - update.currentVersion === target version (dialog still open
 *               until user explicitly refreshes)
 */
type ApplyPhase = "idle" | "confirming" | "applying" | "timeout" | "done";

const POLL_INTERVAL_MS = 5_000;
const POLL_MAX_INTERVAL_MS = 30_000;
const APPLY_TIMEOUT_MS = 5 * 60 * 1_000;
const APPLY_REQUEST_TIMEOUT_MS = 5_000;

function normalizeVersion(version?: string | null): string | undefined {
  const normalized = version?.trim().replace(/^v/, "");
  return normalized || undefined;
}

export function VersionStatus({
  update,
  loading,
  error,
  onRetry,
}: VersionStatusProps) {
  const [phase, setPhase] = useState<ApplyPhase>("idle");
  const [applyTargetVersion, setApplyTargetVersion] = useState<string>();
  const latest = update?.latest;
  const latestVersion = normalizeVersion(latest?.version);
  const serverTargetVersion = update?.updateInProgress
    ? normalizeVersion(update.targetVersion)
    : undefined;
  const targetVersion =
    normalizeVersion(applyTargetVersion) ?? serverTargetVersion ?? latestVersion;
  const consecutiveErrorsRef = useRef(0);

  // Resume an update that was already running before this page loaded.
  useEffect(() => {
    if (phase !== "idle" || !update?.updateInProgress) return;
    const target = normalizeVersion(update.targetVersion);
    if (!target) return;
    setApplyTargetVersion(target);
    setPhase("applying");
  }, [phase, update?.targetVersion, update?.updateInProgress]);

  // Detect success: poll handler will trigger this through onRetry/update.
  useEffect(() => {
    if (phase !== "applying" && phase !== "timeout") return;
    if (!update || !targetVersion) return;
    if (normalizeVersion(update.currentVersion) === targetVersion) {
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

  const isBlocking =
    phase === "applying" || phase === "timeout" || phase === "done";

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
              v{normalizeVersion(update.currentVersion) ?? update.currentVersion}
            </p>
          </div>
          {canRetry && phase === "idle" ? (
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
              onRefresh={() => window.location.reload()}
              onRetryContinue={() => {
                setPhase("applying");
                void onRetry();
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
  phase: "applying" | "timeout" | "done";
  targetVersion: string;
  onRefresh: () => void;
  onRetryContinue: () => void;
};

function UpdateBlockingDialog({
  phase,
  targetVersion,
  onRefresh,
  onRetryContinue,
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
                : phase === "timeout"
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
      {loading ? "Checking…" : "Check again"}
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
