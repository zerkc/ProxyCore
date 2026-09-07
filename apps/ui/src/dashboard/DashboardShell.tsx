import { Link, useLocation } from "react-router-dom";
import type { ReactNode } from "react";
import { useDashboard } from "./dashboard-context";
import { dashboardNav, resolveDashboardNav } from "./nav";
import { RecordDialog } from "./RecordDialog";
import { ZoneDialog } from "./dns/ZoneDialog";
import { VersionStatus } from "./VersionStatus";
import { deriveBarState } from "./patch-bar-state";

export default function DashboardShell({ children }: { children: ReactNode }) {
  const pathname = useLocation().pathname;
  const current = resolveDashboardNav(pathname);
  const {
    status,
    user,
    message,
    error,
    activeZone,
    editingRecord,
    recordDialogOpen,
    closeRecordDialog,
    saveRecord,
    zoneDialogOpen,
    openZoneDialog,
    closeZoneDialog,
    createZone,
    apply,
    logout,
    update,
    updateLoading,
    updateError,
    refreshUpdate,
    inSync,
    failedJob,
    failureReason,
    autoRetrying,
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

  const pendingJob =
    status?.jobs.some((job) =>
      ["queued", "validating", "applying"].includes(job.status),
    ) ?? false;

  return (
    <main className="min-h-screen">
      <TopBar
        user={user}
        loaded={status !== undefined}
        desired={status?.desiredRevision}
        applied={status?.appliedRevision}
        inSync={inSync}
        pendingJob={pendingJob}
        failureReason={failedJob ? failureReason : undefined}
        autoRetrying={autoRetrying}
        onApply={apply}
        onLogout={logout}
      />

      <div className="mx-auto grid min-h-[calc(100vh-3.5rem)] max-w-[1500px] grid-cols-1 border-x border-line/80 lg:grid-cols-[240px_1fr]">
        <aside className="border-b border-line/80 bg-panel/70 p-6 backdrop-blur-sm lg:border-b-0 lg:border-r">
          <VersionStatus
            update={update}
            loading={updateLoading}
            error={updateError}
            onRetry={() => void refreshUpdate()}
          />
          <nav
            className="mt-8 grid grid-cols-2 gap-1.5 sm:grid-cols-3 lg:grid-cols-1"
            aria-label="Main navigation"
          >
            {dashboardNav.map((item) => (
              <Link
                key={item.href}
                to={item.href}
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
                to="/dashboard/dns"
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
      <ZoneDialog
        open={zoneDialogOpen}
        onClose={closeZoneDialog}
        onSubmit={async (name) => {
          const ok = await createZone(name);
          if (ok) closeZoneDialog();
          return ok;
        }}
      />
    </main>
  );
}

// ─── TopBar ──────────────────────────────────────────────────────────────────

type TopBarProps = {
  user: string;
  loaded: boolean;
  desired?: { revisionNumber: number; checksum: string };
  applied?: { revisionNumber: number; checksum: string };
  inSync: boolean;
  pendingJob: boolean;
  failureReason?: string;
  autoRetrying: boolean;
  onApply: () => void;
  onLogout: () => void;
};

function TopBar({
  user,
  loaded,
  desired,
  applied,
  inSync,
  pendingJob,
  failureReason,
  autoRetrying,
  onApply,
  onLogout,
}: TopBarProps) {
  const state = deriveBarState({ loaded, pendingJob, inSync, failureReason });

  const revisionLabel =
    desired && applied
      ? `r${desired.revisionNumber} → r${applied.revisionNumber}`
      : desired
        ? `r${desired.revisionNumber} → —`
        : loaded
          ? "No revision yet"
          : "…";

  const statusLabel =
    state.kind === "checking"
      ? "Checking patch state"
      : state.kind === "applying"
        ? "Apply in progress"
        : state.kind === "failed"
          ? `Failed: ${state.reason}`
          : state.kind === "pending"
            ? "Patch pending"
            : "Patch live";

  const actionLabel = !loaded
    ? "Checking apply state"
    : inSync
      ? "Re-apply current state"
      : "Apply pending changes";

  return (
    <header className="pc-topbar" role="banner">
      {/* Left: brand */}
      <div className="pc-topbar-brand">
        <span className="pc-title text-lg text-mist">ProxyCore</span>
      </div>

      {/* Center: bell + revision status */}
      <div className="pc-topbar-center">
        <BellButton
          state={state}
          loaded={loaded}
          autoRetrying={autoRetrying}
          statusLabel={statusLabel}
          actionLabel={actionLabel}
          onApply={onApply}
        />

        <span
          className="pc-topbar-status hidden text-xs font-semibold md:inline"
          data-kind={state.kind}
        >
          {statusLabel}
        </span>

        <span className="hidden font-mono text-[11px] text-faint lg:inline">
          {revisionLabel}
        </span>

        {desired?.checksum ? (
          <span className="hidden min-w-0 truncate font-mono text-[10px] text-faint/80 xl:inline">
            {desired.checksum.slice(0, 12)}
            {applied?.checksum && desired.checksum !== applied.checksum
              ? ` · ${applied.checksum.slice(0, 12)}`
              : ""}
          </span>
        ) : null}

        <span className="font-mono text-[11px] text-faint lg:hidden">
          {revisionLabel}
        </span>
      </div>

      {/* Right: user + sign-out */}
      <div className="pc-topbar-user">
        {user ? (
          <span
            className="pc-topbar-user-identity"
            title={`Signed in as ${user}`}
          >
            <span className="pc-topbar-user-avatar" aria-hidden="true">
              {user.charAt(0).toUpperCase()}
            </span>
            <span className="hidden text-xs text-mute sm:inline">{user}</span>
          </span>
        ) : null}
        <button
          type="button"
          onClick={onLogout}
          className="text-xs text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
        >
          Sign out
        </button>
      </div>
    </header>
  );
}

// ─── BellButton ───────────────────────────────────────────────────────────────

type BarState = ReturnType<typeof deriveBarState>;

type BellButtonProps = {
  state: BarState;
  loaded: boolean;
  autoRetrying: boolean;
  statusLabel: string;
  actionLabel: string;
  onApply: () => void;
};

function BellButton({
  state,
  loaded,
  autoRetrying,
  statusLabel,
  actionLabel,
  onApply,
}: BellButtonProps) {
  const disabled = !loaded || autoRetrying;

  return (
    <div className="relative inline-flex items-center">
      <button
        type="button"
        onClick={onApply}
        disabled={disabled}
        aria-label={`${actionLabel}. ${statusLabel}`}
        title={`${actionLabel}. ${statusLabel}`}
        className={[
          "pc-topbar-bell",
          state.kind === "applying" || state.kind === "checking"
            ? "pc-topbar-bell-active"
            : state.kind === "failed"
              ? "pc-topbar-bell-failed"
              : state.kind === "pending"
                ? "pc-topbar-bell-pending"
                : "pc-topbar-bell-live",
          disabled ? "pc-topbar-bell-disabled" : "",
        ]
          .filter(Boolean)
          .join(" ")}
      >
        {/* Bell SVG */}
        <svg
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
          className="pc-topbar-bell-icon"
        >
          <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        </svg>

        {/* Status dot badge */}
        <span
          className="pc-topbar-bell-badge"
          data-kind={
            state.kind === "checking"
              ? "checking"
              : state.kind === "applying"
                ? "applying"
                : state.kind === "failed"
                  ? "failed"
                  : state.kind === "pending"
                    ? "pending"
                    : "live"
          }
          aria-hidden="true"
        />
      </button>

      {/* Accessible status for screen readers */}
      <span className="sr-only" role="status" aria-live="polite">
        {statusLabel}
      </span>
    </div>
  );
}
