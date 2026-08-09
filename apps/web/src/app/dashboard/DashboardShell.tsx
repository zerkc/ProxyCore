"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { useDashboard } from "./dashboard-context";
import { dashboardNav, resolveDashboardNav } from "./nav";
import { RecordDialog } from "./RecordDialog";

export default function DashboardShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const current = resolveDashboardNav(pathname);
  const {
    status,
    message,
    error,
    activeZone,
    editingRecord,
    recordDialogOpen,
    closeRecordDialog,
    saveRecord,
    apply,
    logout,
    inSync,
  } = useDashboard();

  const zoneRouteMatch = pathname.match(/^\/dashboard\/dns\/([^/]+)\/?$/);
  const routeZone = zoneRouteMatch
    ? status?.zones.find((zone) => zone.id === zoneRouteMatch[1])
    : undefined;
  const headerEyebrow = routeZone
    ? "DNS & proxy · working zone"
    : current.id === "dns"
      ? "DNS & proxy"
      : "Installation / local";
  const headerTitle = routeZone?.name ?? current.title;
  const headerSubtitle = routeZone
    ? `${routeZone.records.length} DNS ${
        routeZone.records.length === 1 ? "record" : "records"
      } in this namespace`
    : current.id === "dns"
      ? "Pick a zone, then manage its hostnames and proxy settings."
      : current.id === "ingress"
        ? "Advertised address and default resolver for DNS answers."
        : current.id === "streams"
          ? "TCP and UDP listeners forwarded to upstream hosts."
          : "See what is intended, what is applied, and what still needs an operator's hand.";

  return (
    <main className="min-h-screen">
      <PatchBar
        desired={status?.desiredRevision}
        applied={status?.appliedRevision}
        inSync={inSync}
        onApply={apply}
      />

      <div className="mx-auto grid min-h-[calc(100vh-2.5rem)] max-w-[1500px] grid-cols-1 border-x border-line/80 lg:grid-cols-[240px_1fr]">
        <aside className="border-b border-line/80 bg-panel/70 p-6 backdrop-blur-sm lg:border-b-0 lg:border-r">
          <div className="flex items-center justify-between lg:block">
            <div>
              <p className="pc-title text-2xl text-mist">ProxyCore</p>
              <p className="mt-1 text-xs text-mute">Night-shift network desk</p>
            </div>
            <button
              className="text-xs text-mute underline decoration-line underline-offset-4 transition hover:text-mist lg:mt-10"
              onClick={logout}
            >
              Sign out
            </button>
          </div>
          <nav
            className="mt-8 grid grid-cols-2 gap-1.5 sm:grid-cols-3 lg:grid-cols-1"
            aria-label="Main navigation"
          >
            {dashboardNav.map((item) => (
              <Link
                key={item.href}
                href={item.href}
                data-active={current.id === item.id}
                className="pc-nav"
              >
                {item.label}
              </Link>
            ))}
          </nav>
          {routeZone ? (
            <div className="mt-10 hidden rounded-xl border border-signal/30 bg-signal/10 p-4 lg:block">
              <p className="pc-eyebrow pc-eyebrow-signal">Now editing</p>
              <p className="mt-2 break-all font-mono text-sm text-mist">
                {routeZone.name}
              </p>
              <Link
                href="/dashboard/dns"
                className="mt-3 inline-block text-xs text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
              >
                Back to zones
              </Link>
            </div>
          ) : (
            <div className="mt-10 hidden lg:block">
              <p className="pc-eyebrow">Apply discipline</p>
              <p className="mt-3 text-sm leading-6 text-mute">
                Saving a zone or record queues an apply immediately. Use the
                manual action only to re-apply the current desired state.
              </p>
            </div>
          )}
        </aside>

        <section className="p-6 md:p-10">
          <header className="flex flex-col justify-between gap-4 border-b border-line/80 pb-8 md:flex-row md:items-end">
            <div>
              <p className="pc-eyebrow">{headerEyebrow}</p>
              <h1
                className={`pc-title mt-3 text-mist md:text-[2.75rem] ${
                  routeZone
                    ? "break-all font-mono text-3xl md:text-4xl"
                    : "text-4xl"
                }`}
              >
                {headerTitle}
              </h1>
              <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
                {headerSubtitle}
              </p>
            </div>
          </header>

          {message ? (
            <p className="pc-toast-ok" role="status">
              {message}
            </p>
          ) : null}
          {error ? (
            <p className="pc-toast-err" role="alert">
              {error}
            </p>
          ) : null}

          <div key={current.href} className="pc-enter">
            {children}
          </div>
        </section>
      </div>

      <RecordDialog
        open={recordDialogOpen}
        zoneName={activeZone?.name}
        certificates={status?.certificates ?? []}
        initial={editingRecord}
        onClose={closeRecordDialog}
        onSubmit={saveRecord}
      />
    </main>
  );
}

function PatchBar({
  desired,
  applied,
  inSync,
  onApply,
}: {
  desired?: { revisionNumber: number; checksum: string };
  applied?: { revisionNumber: number; checksum: string };
  inSync: boolean;
  onApply: () => void;
}) {
  const drift = !inSync;
  const revisionLabel =
    desired && applied
      ? `r${desired.revisionNumber} → r${applied.revisionNumber}`
      : desired
        ? `r${desired.revisionNumber} → —`
        : "No revision yet";

  return (
    <div
      className="pc-sync-bar"
      data-drift={drift ? "true" : "false"}
      role="status"
      aria-live="polite"
    >
      <div className="pc-sync-bar-inner mx-auto w-full max-w-[1500px]">
        <span className="pc-sync-dot" aria-hidden />
        <span
          className={`shrink-0 text-xs font-semibold ${
            drift ? "text-signal" : "text-link"
          }`}
        >
          {drift ? "Patch pending" : "Patch live"}
        </span>
        <span className="hidden font-mono text-[11px] text-faint sm:inline">
          {revisionLabel}
        </span>
        {desired?.checksum ? (
          <span className="hidden min-w-0 truncate font-mono text-[10px] text-faint/80 md:inline">
            {desired.checksum.slice(0, 12)}
            {applied?.checksum && desired.checksum !== applied.checksum
              ? ` · ${applied.checksum.slice(0, 12)}`
              : ""}
          </span>
        ) : null}
        <div className="ml-auto flex items-center gap-2">
          <span className="font-mono text-[11px] text-faint sm:hidden">
            {revisionLabel}
          </span>
          <button
            type="button"
            onClick={onApply}
            className={`rounded-lg px-2.5 py-1 text-[11px] font-semibold transition ${
              drift
                ? "bg-signal text-[#1a120c] hover:bg-[#e6893d]"
                : "border border-line text-mute hover:border-signal/40 hover:text-mist"
            }`}
          >
            {drift ? "Apply now" : "Re-apply"}
          </button>
        </div>
      </div>
      <div className="pc-sync-cable" aria-hidden />
    </div>
  );
}
