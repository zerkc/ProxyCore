"use client";

import Link from "next/link";
import type { EditableRecord } from "../RecordDialog";
import type { Zone } from "../types";

export function ZoneRecordsView(props: {
  zone?: Zone;
  openCreate: () => void;
  openEdit: (record: EditableRecord) => void;
}) {
  if (!props.zone) {
    return (
      <div className="mt-8 space-y-4">
        <Link
          href="/dashboard/dns"
          className="inline-flex text-sm text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
        >
          ← All zones
        </Link>
        <p className="pc-panel p-6 text-sm text-faint">
          That zone was not found. It may have been removed.
        </p>
      </div>
    );
  }

  const { zone } = props;
  const proxiedCount = zone.records.filter((record) => record.proxied).length;

  return (
    <div className="mt-8 space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <nav aria-label="Zone breadcrumb" className="flex flex-wrap items-center gap-2 text-sm">
          <Link
            href="/dashboard/dns"
            className="text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
          >
            Zones
          </Link>
          <span className="text-faint" aria-hidden>
            /
          </span>
          <span className="font-mono text-signal">{zone.name}</span>
        </nav>
        <button className="pc-btn-soft" type="button" onClick={props.openCreate}>
          Add DNS record
        </button>
      </div>

      <div className="flex flex-wrap gap-3 text-xs">
        <span className="rounded-lg border border-line bg-bay/60 px-3 py-1.5 font-mono text-mute">
          {zone.records.length}{" "}
          {zone.records.length === 1 ? "record" : "records"}
        </span>
        <span className="rounded-lg border border-line bg-bay/60 px-3 py-1.5 font-mono text-mute">
          {proxiedCount} proxied
        </span>
      </div>

      <section className="space-y-2">
        {zone.records.length ? (
          zone.records.map((record) => (
            <div
              key={record.id}
              className="flex items-center justify-between gap-3 rounded-xl border border-line/80 bg-bay/50 px-4 py-3.5 text-sm"
            >
              <div className="min-w-0">
                <p className="truncate font-mono text-mist">{record.name}</p>
                <p className="mt-1 font-mono text-xs text-faint">
                  {record.type} /{" "}
                  {record.proxied ? summarizeProxy(record) : "DNS-only"}
                </p>
              </div>
              <button
                type="button"
                className="pc-btn-ghost shrink-0 !px-3 !py-1.5 !text-xs"
                onClick={() => props.openEdit(record)}
              >
                Configure
              </button>
            </div>
          ))
        ) : (
          <p className="rounded-xl border border-dashed border-line p-6 text-sm text-faint">
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
