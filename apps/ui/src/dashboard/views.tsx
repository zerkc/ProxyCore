import { deriveBarState, selectRelevantFailedJob } from "./patch-bar-state";
import type { StatusPayload } from "./types";

const activeJobStatuses = new Set(["queued", "validating", "applying"]);

export function Overview({ status }: { status?: StatusPayload }) {
  const jobs = status?.jobs ?? [];
  const desired = status?.desiredRevision;
  const applied = status?.appliedRevision;
  const pendingJob = jobs.some((job) => activeJobStatuses.has(job.status));
  const inSync = Boolean(
    desired && applied && desired.checksum === applied.checksum,
  );
  const failedJob = selectRelevantFailedJob(jobs);
  const state = deriveBarState({
    loaded: status !== undefined,
    pendingJob,
    inSync,
    failureReason: failedJob?.errorMessage ?? undefined,
  });
  const stateLabel =
    state.kind === "checking"
      ? "checking"
      : state.kind === "applying"
        ? "apply in progress"
        : state.kind === "failed"
          ? "apply failed"
          : state.kind === "pending"
            ? "changes pending"
            : "live";
  const stateTone =
    state.kind === "failed"
      ? "text-danger"
      : state.kind === "live"
        ? "text-signal"
        : state.kind === "checking"
          ? "text-mute"
          : "text-link";
  const stateDescription =
    state.kind === "checking"
      ? "Reading the latest desired and applied revisions."
      : state.kind === "applying"
        ? "A worker is validating or applying the desired revision."
        : state.kind === "failed"
          ? state.reason
          : state.kind === "pending"
            ? "The desired revision is ahead of the last applied revision."
            : "Desired and applied checksums match.";
  const recentJobs = [...jobs]
    .sort((a, b) => jobTimestamp(b.createdAt) - jobTimestamp(a.createdAt))
    .slice(0, 5);

  return (
    <div className="mt-6 space-y-8">
      <section className="border-y border-line/80" aria-labelledby="apply-state-title">
        <div className="flex flex-wrap items-start justify-between gap-4 border-b border-line/80 px-1 py-4">
          <div>
            <p className="pc-eyebrow">apply desk</p>
            <h2 id="apply-state-title" className="pc-title mt-2 text-2xl text-mist">
              Desired state
            </h2>
          </div>
          <span className={`border border-line px-3 py-1.5 font-mono text-xs uppercase tracking-[0.1em] ${stateTone}`}>
            {stateLabel}
          </span>
        </div>

        <div className="grid gap-4 px-1 py-5 md:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] md:items-center">
          <RevisionCell label="desired" revision={desired} />
          <span className="hidden font-mono text-faint md:block" aria-hidden>
            →
          </span>
          <RevisionCell label="applied" revision={applied} muted={!applied} />
        </div>
        <p className={`border-t border-line/80 px-1 py-3 text-sm ${state.kind === "failed" ? "text-danger" : "text-mute"}`}>
          {stateDescription}
        </p>
      </section>

      <div className="grid gap-8 lg:grid-cols-[minmax(0,1.4fr)_minmax(16rem,0.6fr)]">
        <section className="border-y border-line/80" aria-labelledby="change-queue-title">
          <header className="flex items-end justify-between gap-4 border-b border-line/80 px-1 py-4">
            <div>
              <p className="pc-eyebrow">change queue</p>
              <h2 id="change-queue-title" className="pc-title mt-2 text-xl text-mist">
                Recent apply jobs
              </h2>
            </div>
            <span className="font-mono text-xs text-faint">{jobs.length} total</span>
          </header>
          {recentJobs.length ? (
            <div>
              {recentJobs.map((job) => (
                <article
                  key={job.id}
                  className="grid gap-2 border-b border-line/80 px-1 py-3.5 last:border-b-0 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-center"
                >
                  <div className="min-w-0">
                    <p className="truncate font-mono text-sm text-mist">
                      {job.target || "configuration apply"}
                    </p>
                    <p className="mt-1 truncate font-mono text-[11px] text-faint">
                      job {job.id}
                    </p>
                  </div>
                  <span
                    className={`font-mono text-[11px] uppercase tracking-[0.1em] ${job.status === "failed" ? "text-danger" : activeJobStatuses.has(job.status) ? "text-signal" : "text-faint"}`}
                  >
                    {job.status}
                  </span>
                  <time className="font-mono text-[11px] text-faint sm:text-right" dateTime={job.createdAt}>
                    {formatJobTime(job.createdAt)}
                  </time>
                  {job.errorMessage ? (
                    <p className="text-xs leading-5 text-danger sm:col-span-3">
                      {job.errorMessage}
                    </p>
                  ) : null}
                </article>
              ))}
            </div>
          ) : (
            <p className="border-b border-dashed border-line px-1 py-5 text-sm text-faint">
              No apply jobs yet. Saving the first change will create one here.
            </p>
          )}
        </section>

        <section className="border-y border-line/80" aria-labelledby="last-good-title">
          <header className="border-b border-line/80 px-1 py-4">
            <p className="pc-eyebrow">last known good</p>
            <h2 id="last-good-title" className="pc-title mt-2 text-xl text-mist">
              Applied revision
            </h2>
          </header>
          <div className="space-y-4 px-1 py-5">
            <div>
              <p className="font-mono text-2xl text-link">
                {applied ? `r${applied.revisionNumber}` : "—"}
              </p>
              <p className="mt-1 font-mono text-[11px] text-faint">
                {applied ? applied.checksum.slice(0, 16) : "No applied revision yet"}
              </p>
            </div>
            {failedJob ? (
              <div className="border-t border-danger/40 pt-4">
                <p className="pc-eyebrow text-danger">attention</p>
                <p className="mt-2 text-sm leading-5 text-danger">
                  {failedJob.errorMessage ?? "The latest apply failed."}
                </p>
              </div>
            ) : (
              <p className="border-t border-line/80 pt-4 text-sm leading-5 text-mute">
                The apply control is in the topbar. Use it to promote the desired revision when changes are pending.
              </p>
            )}
          </div>
        </section>
      </div>
    </div>
  );
}

function RevisionCell(props: {
  label: string;
  revision?: { revisionNumber: number; checksum: string };
  muted?: boolean;
}) {
  return (
    <div className={props.muted ? "opacity-60" : ""}>
      <p className="font-mono text-[11px] uppercase tracking-[0.1em] text-faint">
        {props.label}
      </p>
      <p className="mt-2 font-mono text-2xl text-mist">
        {props.revision ? `r${props.revision.revisionNumber}` : "—"}
      </p>
      <p className="mt-1 truncate font-mono text-[11px] text-faint">
        {props.revision ? props.revision.checksum.slice(0, 16) : "not available"}
      </p>
    </div>
  );
}

function jobTimestamp(value: string): number {
  const timestamp = Date.parse(value);
  return Number.isNaN(timestamp) ? 0 : timestamp;
}

function formatJobTime(value: string): string {
  const timestamp = Date.parse(value);
  if (Number.isNaN(timestamp)) return "—";
  return new Date(timestamp).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
