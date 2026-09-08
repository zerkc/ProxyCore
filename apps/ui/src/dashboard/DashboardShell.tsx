import { Link, useLocation } from "react-router-dom";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useDashboard } from "./dashboard-context";
import { dashboardNav, resolveDashboardNav } from "./nav";
import { RecordDialog } from "./RecordDialog";
import { ZoneDialog } from "./dns/ZoneDialog";
import { VersionStatus } from "./VersionStatus";
import { deriveBarState } from "./patch-bar-state";

const NAVIGATION_STORAGE_KEY = "proxycore.navigation.collapsed";

type DashboardNavIcon = (typeof dashboardNav)[number]["icon"];

function getStoredRailCollapsed() {
  if (typeof window === "undefined") return false;

  try {
    return window.localStorage.getItem(NAVIGATION_STORAGE_KEY) === "true";
  } catch {
    return false;
  }
}

function storeRailCollapsed(collapsed: boolean) {
  if (typeof window === "undefined") return;

  try {
    window.localStorage.setItem(NAVIGATION_STORAGE_KEY, String(collapsed));
  } catch {
    // Ignore storage failures; the rail remains usable for the current session.
  }
}

function NavIcon({ icon }: { icon: DashboardNavIcon }) {
  let glyph: ReactNode;

  switch (icon) {
    case "overview":
      glyph = (
        <path d="M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z" />
      );
      break;
    case "dns":
      glyph = (
        <>
          <circle cx="12" cy="12" r="8" />
          <path d="M4 12h16M12 4a12 12 0 0 1 0 16M12 4a12 12 0 0 0 0 16" />
        </>
      );
      break;
    case "certificates":
      glyph = (
        <>
          <path d="m12 3 7 3v5c0 4.5-2.7 7.6-7 10-4.3-2.4-7-5.5-7-10V6l7-3Z" />
          <path d="m9 12 2 2 4-4" />
        </>
      );
      break;
    case "ingress":
      glyph = (
        <>
          <path d="M4 5v14M5 12h14m-5-5 5 5-5 5" />
        </>
      );
      break;
    case "streams":
      glyph = (
        <>
          <circle cx="6" cy="6" r="2" />
          <circle cx="18" cy="12" r="2" />
          <circle cx="6" cy="18" r="2" />
          <path d="m8 7 8 4m-8 6 8-4" />
        </>
      );
      break;
    default:
      glyph = null;
  }

  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      className="pc-command-rail-icon"
    >
      {glyph}
    </svg>
  );
}

export default function DashboardShell({ children }: { children: ReactNode }) {
  const pathname = useLocation().pathname;
  const current = resolveDashboardNav(pathname);
  const [railCollapsed, setRailCollapsed] = useState(() =>
    getStoredRailCollapsed(),
  );
  const toggleRail = () => {
    const nextCollapsed = !railCollapsed;
    setRailCollapsed(nextCollapsed);
    storeRailCollapsed(nextCollapsed);
  };
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

  const pendingJob =
    status?.jobs.some((job) =>
      ["queued", "validating", "applying"].includes(job.status),
    ) ?? false;
  const systemState = deriveBarState({
    loaded: status !== undefined,
    pendingJob,
    inSync,
    failureReason: failedJob ? failureReason : undefined,
  });
  const systemStateLabel =
    systemState.kind === "checking"
      ? "checking"
      : systemState.kind === "applying"
        ? "apply pending"
        : systemState.kind === "failed"
          ? "failed"
          : systemState.kind === "pending"
            ? "apply pending"
            : "live";

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
        railCollapsed={railCollapsed}
        onToggleRail={toggleRail}
      />

      <div className="w-full min-h-[calc(100vh-3.5rem)]">
        <div
          className="pc-command-layout"
          data-collapsed={railCollapsed}
          data-rail-collapsed={railCollapsed}
        >
          <aside className="pc-command-rail" data-collapsed={railCollapsed}>
            {!railCollapsed ? (
              <div className="pc-command-rail-header">
                <p className="pc-eyebrow pc-command-rail-eyebrow">navigate</p>
              </div>
            ) : null}
            <nav className="pc-command-rail-nav" aria-label="Main navigation">
              {dashboardNav.map((item) => (
                <Link
                  key={item.href}
                  to={item.href}
                  data-active={current.id === item.id}
                  aria-current={current.id === item.id ? "page" : undefined}
                  title={item.title}
                  className="pc-command-rail-link"
                >
                  <NavIcon icon={item.icon} />
                  <span className="pc-command-rail-label">{item.label}</span>
                </Link>
              ))}
            </nav>
          </aside>

          <div className="pc-command-content">
            <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1fr)_280px]">
          <section className="min-w-0 p-4 md:p-6">
            <div className="mx-auto w-full max-w-[1120px]">
              {current.id === "overview" ? (
                <header className="border-b border-line/80 pb-4 md:pb-5">
                  <p className="pc-eyebrow">control room</p>
                  <h1 className="pc-title mt-2 text-3xl text-mist">
                    control room
                  </h1>
                  <p className="mt-2 max-w-2xl text-sm leading-6 text-mute">
                    Desired configuration, apply state, and service posture at a
                    glance.
                  </p>
                </header>
              ) : null}

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
            </div>
          </section>

          <aside className="pc-inspector min-w-0 border-t border-line/80 lg:min-h-[calc(100vh-7.25rem)] lg:border-l lg:border-t-0">
            <section className="pc-inspector-section px-4 py-5 md:px-5">
              <p className="pc-eyebrow">system</p>
              <dl className="mt-4 space-y-2.5 font-mono text-xs">
                <div className="flex items-baseline justify-between gap-4 border-b border-line/70 pb-2">
                  <dt className="text-faint">desired</dt>
                  <dd className="text-mist">
                    {status?.desiredRevision
                      ? `r${status.desiredRevision.revisionNumber}`
                      : status
                        ? "—"
                        : "…"}
                  </dd>
                </div>
                <div className="flex items-baseline justify-between gap-4 border-b border-line/70 pb-2">
                  <dt className="text-faint">applied</dt>
                  <dd className="text-mist">
                    {status?.appliedRevision
                      ? `r${status.appliedRevision.revisionNumber}`
                      : status
                        ? "—"
                        : "…"}
                  </dd>
                </div>
                <div className="flex items-baseline justify-between gap-4">
                  <dt className="text-faint">state</dt>
                  <dd
                    className="pc-inspector-state text-right"
                    data-kind={systemState.kind}
                  >
                    {systemStateLabel}
                  </dd>
                </div>
              </dl>
              {systemState.kind === "failed" ? (
                <p className="mt-3 text-xs leading-5 text-danger">
                  {systemState.reason}
                </p>
              ) : null}
              <p className="mt-4 text-xs leading-5 text-mute">
                Apply status lives in the topbar.
              </p>
            </section>

            <section className="pc-inspector-section border-t border-line/80 px-4 py-5 md:px-5">
              {routeZone ? (
                <>
                  <p className="pc-eyebrow pc-eyebrow-signal">Now editing</p>
                  <p className="mt-2 break-all font-mono text-sm text-mist">
                    {routeZone.name}
                  </p>
                  <Link
                    to="/dashboard/dns"
                    className="mt-2 inline-block text-xs text-mute underline decoration-line underline-offset-4 transition hover:text-mist"
                  >
                    Back to zones
                  </Link>
                </>
              ) : (
                <>
                  <p className="pc-eyebrow">Apply discipline</p>
                  <p className="mt-2 text-xs leading-5 text-mute">
                    Saving a zone or record queues an apply immediately. Use the
                    manual action only to re-apply the current desired state.
                  </p>
                </>
              )}
            </section>

            <div className="pc-inspector-version">
              <VersionStatus
                update={update}
                loading={updateLoading}
                error={updateError}
                onRetry={() => void refreshUpdate()}
              />
            </div>
          </aside>
            </div>
          </div>
        </div>
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
  railCollapsed: boolean;
  onToggleRail: () => void;
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
  railCollapsed,
  onToggleRail,
  onApply,
  onLogout,
}: TopBarProps) {
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const userMenuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!userMenuOpen) return;

    function closeOnPointerDown(event: PointerEvent) {
      if (
        userMenuRef.current &&
        !userMenuRef.current.contains(event.target as Node)
      ) {
        setUserMenuOpen(false);
      }
    }

    function closeOnEscape(event: KeyboardEvent) {
      if (event.key === "Escape") {
        setUserMenuOpen(false);
      }
    }

    document.addEventListener("pointerdown", closeOnPointerDown);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOnPointerDown);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [userMenuOpen]);

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
      ? "apply checking"
      : state.kind === "applying"
        ? "apply queued"
        : state.kind === "failed"
          ? `failed · ${state.reason}`
          : state.kind === "pending"
            ? "apply pending"
            : "live";

  const actionLabel = !loaded
    ? "Checking apply state"
    : inSync
      ? "Re-apply current state"
      : "Apply pending changes";
  const railToggleLabel = railCollapsed
    ? "Expand main navigation"
    : "Collapse main navigation";

  return (
    <header className="pc-topbar" role="banner">
      {/* Left: brand */}
      <div className="pc-topbar-brand">
        <button
          type="button"
          className="pc-topbar-rail-toggle"
          onClick={onToggleRail}
          aria-label={railToggleLabel}
          aria-expanded={!railCollapsed}
          title={railToggleLabel}
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.75"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
            focusable="false"
            className="pc-topbar-rail-toggle-icon"
          >
            <path d={railCollapsed ? "m9 5 7 7-7 7" : "m15 5-7 7 7 7"} />
          </svg>
          <span className="sr-only">{railToggleLabel}</span>
        </button>
        <span className="pc-title text-lg lowercase text-mist">proxycore</span>
        <span className="pc-topbar-brand-context hidden text-xs text-mute sm:inline">
          / local control plane
        </span>
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
          <span className="mr-1" aria-hidden="true">●</span>{statusLabel}
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

      {/* Right: user menu */}
      <div className="pc-topbar-user" ref={userMenuRef}>
        {user ? (
          <>
            <button
              type="button"
              className="pc-topbar-user-trigger"
              onClick={() => setUserMenuOpen((open) => !open)}
              aria-haspopup="menu"
              aria-expanded={userMenuOpen}
              aria-controls="topbar-user-menu"
              title={`Signed in as ${user}`}
            >
              <span className="pc-topbar-user-avatar" aria-hidden="true">
                {user.charAt(0).toUpperCase()}
              </span>
              <span className="hidden text-xs text-mute sm:inline">{user}</span>
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.75"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
                className="pc-topbar-user-chevron"
              >
                <path d="m6 9 6 6 6-6" />
              </svg>
            </button>
            {userMenuOpen ? (
              <div
                id="topbar-user-menu"
                className="pc-topbar-user-menu"
                role="menu"
                aria-label="User menu"
              >
                <div className="pc-topbar-user-menu-meta">
                  <p className="pc-eyebrow">session</p>
                  <p className="mt-1 truncate font-mono text-xs text-mist">
                    {user}
                  </p>
                </div>
                <button
                  type="button"
                  role="menuitem"
                  className="pc-topbar-user-menu-item"
                  onClick={() => {
                    setUserMenuOpen(false);
                    onLogout();
                  }}
                >
                  Sign out
                </button>
              </div>
            ) : null}
          </>
        ) : null}
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
