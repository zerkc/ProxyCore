import type { UpdatePayload } from "./types";

type VersionStatusProps = {
  update?: UpdatePayload;
  loading: boolean;
  error: string;
  onRetry: () => void;
};

export function VersionStatus({
  update,
  loading,
  error,
  onRetry,
}: VersionStatusProps) {
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

  const latest = update.latest;
  const hasUpdate = update.status === "update_available" && latest;
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
        {canRetry ? <RetryButton loading={loading} onRetry={onRetry} /> : null}
      </div>
      {hasUpdate ? (
        <a
          href={latest.url}
          target="_blank"
          rel="noreferrer"
          className="mt-3 block rounded-xl border border-signal/30 bg-signal/10 p-3 transition hover:border-signal/60"
        >
          <span className="pc-eyebrow pc-eyebrow-signal">Update available</span>
          <span className="mt-2 block font-mono text-sm text-mist">
            v{latest.version} <span aria-hidden>↗</span>
          </span>
        </a>
      ) : (
        <p className="mt-2 text-xs leading-5 text-mute">{statusMessage}</p>
      )}
      {error ? <p className="mt-2 text-xs text-mute">{error}</p> : null}
    </section>
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
