"use client";

import Link from "next/link";
import type { FormEvent } from "react";
import type { StatusPayload } from "../types";

export function DnsZonesView(props: {
  status?: StatusPayload;
  zoneName: string;
  setZoneName: (value: string) => void;
  createZone: (event: FormEvent<HTMLFormElement>) => void;
}) {
  const zones = props.status?.zones ?? [];

  return (
    <div className="mt-8 space-y-6">
      <section className="pc-panel p-6 md:p-8">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <p className="pc-eyebrow">Zones</p>
            <h2 className="pc-title mt-2 text-2xl text-mist">
              Namespaces you own
            </h2>
            <p className="mt-2 max-w-xl text-sm leading-6 text-mute">
              Open a zone to manage its DNS records and proxy settings. Each
              zone is one authoritative namespace.
            </p>
          </div>
          <p className="font-mono text-sm text-link">
            {zones.length} {zones.length === 1 ? "zone" : "zones"}
          </p>
        </div>

        <form
          className="mt-6 flex flex-col gap-2 sm:flex-row"
          onSubmit={props.createZone}
        >
          <input
            value={props.zoneName}
            onChange={(event) => props.setZoneName(event.target.value)}
            className="pc-input mt-0 min-w-0 flex-1"
            aria-label="Zone name"
            placeholder="home.arpa"
          />
          <button className="pc-btn shrink-0" type="submit">
            Add zone
          </button>
        </form>
      </section>

      <section className="space-y-2">
        {zones.length ? (
          zones.map((zone) => (
            <Link
              key={zone.id}
              href={`/dashboard/dns/${zone.id}`}
              className="group flex items-center justify-between gap-4 rounded-xl border border-line/80 bg-panel/70 px-5 py-4 transition hover:border-signal/40 hover:bg-signal/10"
            >
              <div className="min-w-0">
                <p className="truncate font-mono text-base text-mist group-hover:text-signal">
                  {zone.name}
                </p>
                <p className="mt-1 text-xs text-faint">
                  {zone.records.length}{" "}
                  {zone.records.length === 1 ? "record" : "records"}
                </p>
              </div>
              <span className="shrink-0 text-sm text-mute transition group-hover:text-signal">
                Open →
              </span>
            </Link>
          ))
        ) : (
          <p className="rounded-xl border border-dashed border-line p-6 text-sm text-faint">
            No zones yet. Add the namespace your homelab owns, then open it to
            create DNS records.
          </p>
        )}
      </section>
    </div>
  );
}
