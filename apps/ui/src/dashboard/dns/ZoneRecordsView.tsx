import { Link } from "react-router-dom";
import type { EditableRecord } from "../RecordDialog";
import type { Zone } from "../types";

export function ZoneRecordsView(props: {
  zone?: Zone;
  openCreate: () => void;
  openEdit: (record: EditableRecord) => void;
}) {
  if (!props.zone) {
    return (
      <div className="mt-6 space-y-4">
        <Link
          to="/dashboard/dns"
          className="inline-flex text-sm text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
        >
          ← All zones
        </Link>
        <p className="border-b border-dashed border-line p-5 text-sm text-faint">
          That zone was not found. It may have been removed.
        </p>
      </div>
    );
  }

  const { zone } = props;
  const proxiedCount = zone.records.filter((record) => record.proxied).length;

  return (
    <div className="mt-6 space-y-5">
      <header className="border-b border-line/80 pb-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <nav
            aria-label="Zone breadcrumb"
            className="flex flex-wrap items-center gap-2 text-sm"
          >
            <Link
              to="/dashboard/dns"
              className="text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
            >
              Zones
            </Link>
            <span className="text-faint" aria-hidden>
              /
            </span>
            <span className="font-mono text-signal">{zone.name}</span>
          </nav>
          <button className="pc-btn" type="button" onClick={props.openCreate}>
            Add DNS record
          </button>
        </div>
        <div className="mt-4 flex flex-wrap gap-x-5 gap-y-2 font-mono text-xs text-faint">
          <span>
            <span className="text-mute">{zone.records.length}</span>{" "}
            {zone.records.length === 1 ? "record" : "records"}
          </span>
          <span>
            <span className="text-mute">{proxiedCount}</span> proxied
          </span>
        </div>
      </header>

      <section className="border-t border-line/80">
        {zone.records.length ? (
          zone.records.map((record) => (
            <div
              key={record.id}
              className="grid gap-3 border-b border-line/80 px-1 py-3.5 text-sm sm:px-2 md:grid-cols-[minmax(0,1fr)_minmax(12rem,0.8fr)_auto] md:items-center"
            >
              <div className="min-w-0">
                <p className="truncate font-mono text-mist">{record.name}</p>
              </div>
              <p className="truncate font-mono text-xs text-faint">
                <span className="text-link">{record.type}</span> /{" "}
                {record.proxied ? summarizeProxy(record) : "DNS-only"}
              </p>
              <button
                type="button"
                className="pc-btn-ghost justify-self-start !px-3 !py-1.5 !text-xs md:justify-self-end"
                onClick={() => props.openEdit(record)}
              >
                Configure
              </button>
            </div>
          ))
        ) : (
          <p className="border-b border-dashed border-line p-5 text-sm text-faint">
            No records in {zone.name} yet. Add the first hostname for this
            namespace.
          </p>
        )}
      </section>
    </div>
  );
}

function summarizeProxy(record: EditableRecord): string {
  const origin = record.proxy?.origin;
  if (!origin?.ip || !origin.port) return "proxied";
  return `proxied → ${origin.ip}:${origin.port}`;
}
