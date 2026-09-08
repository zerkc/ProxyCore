import { Link } from "react-router-dom";
import type { StatusPayload } from "../types";

export function DnsZonesView(props: {
  status?: StatusPayload;
  openCreate: () => void;
}) {
  const zones = props.status?.zones ?? [];

  return (
    <div className="mt-6 space-y-5">
      <header className="flex flex-col gap-4 border-b border-line/80 pb-5 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <p className="pc-eyebrow">zones</p>
          <h2 className="pc-title mt-2 text-2xl text-mist">
            Namespaces you own
          </h2>
          <p className="mt-2 max-w-xl text-sm leading-6 text-mute">
            Open a zone to manage its DNS records and proxy settings. Each zone
            is one authoritative namespace.
          </p>
        </div>
        <div className="flex shrink-0 items-center justify-between gap-4 sm:justify-end">
          <p className="font-mono text-xs text-link">
            {zones.length} {zones.length === 1 ? "zone" : "zones"}
          </p>
          <button
            type="button"
            className="pc-btn"
            onClick={props.openCreate}
            aria-label="Add zone"
          >
            Add zone
          </button>
        </div>
      </header>

      <section className="border-t border-line/80">
        {zones.length ? (
          zones.map((zone) => (
            <Link
              key={zone.id}
              to={`/dashboard/dns/${zone.id}`}
              className="group flex items-center justify-between gap-4 border-b border-line/80 px-1 py-4 transition hover:bg-raised/60 sm:px-2"
            >
              <div className="min-w-0">
                <p className="truncate font-mono text-base text-mist transition group-hover:text-signal">
                  {zone.name}
                </p>
                <p className="mt-1 text-xs text-faint">
                  {zone.records.length}{" "}
                  {zone.records.length === 1 ? "record" : "records"}
                </p>
              </div>
              <span className="shrink-0 font-mono text-xs text-mute transition group-hover:text-signal">
                Open →
              </span>
            </Link>
          ))
        ) : (
          <p className="border-b border-dashed border-line p-5 text-sm text-faint">
            No zones yet. Add the namespace your homelab owns, then open it to
            create DNS records.
          </p>
        )}
      </section>
    </div>
  );
}
