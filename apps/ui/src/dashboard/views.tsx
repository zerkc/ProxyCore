import type { StatusPayload } from "./types";

export function Overview({ status }: { status?: StatusPayload }) {
  const cards = [
    [
      "CoreDNS",
      status?.zones.length
        ? `${status.zones.length} managed zones`
        : "No zones yet",
    ],
    [
      "Nginx",
      status?.appliedRevision
        ? "Serving last applied revision"
        : "Awaiting first apply",
    ],
    [
      "Worker",
      status?.jobs.some((job) => job.status === "queued")
        ? "Apply queued"
        : "Standing by",
    ],
  ];
  return (
    <div className="mt-8 space-y-8">
      <div className="grid gap-3 md:grid-cols-3">
        {cards.map(([label, value]) => (
          <article key={label} className="pc-panel p-5">
            <p className="pc-eyebrow">{label}</p>
            <p className="mt-5 font-mono text-lg text-link">{value}</p>
          </article>
        ))}
      </div>
      <div className="pc-panel p-6">
        <p className="pc-eyebrow">The safe path</p>
        <div className="mt-5 flex flex-wrap gap-x-6 gap-y-3 text-sm text-mute">
          {["Desired", "Revision", "Validate", "Promote", "Healthy"].map(
            (step, index) => (
              <div key={step} className="flex items-center gap-2.5">
                <span className="font-mono text-[11px] text-signal">
                  {String(index + 1).padStart(2, "0")}
                </span>
                <span className="text-mist/90">{step}</span>
                {index < 4 ? (
                  <span className="hidden text-faint sm:inline" aria-hidden>
                    →
                  </span>
                ) : null}
              </div>
            ),
          )}
        </div>
      </div>
    </div>
  );
}

export function OperatorsView() {
  return (
    <section className="mt-8 pc-panel p-6">
      <p className="pc-eyebrow">Local identities</p>
      <h2 className="pc-title mt-4 text-2xl text-mist">
        Owner / Operator access
      </h2>
      <p className="mt-3 max-w-xl text-sm leading-6 text-mute">
        User management is intentionally server-authorized and audit-backed. Use
        the API endpoint to create the first Operator while the dedicated table
        view is being expanded.
      </p>
      <code className="mt-6 block rounded-xl bg-bay px-4 py-3 font-mono text-xs text-link">
        POST /api/users
      </code>
    </section>
  );
}
