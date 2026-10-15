/** Moves focus to an existing control, scrolling it into view when possible. */
function focusControl(selector: string) {
  const element = document.querySelector<HTMLElement>(selector);
  if (!element) return;
  element.scrollIntoView?.({ block: "center" });
  element.focus();
}

/**
 * Shown on a NODE only when its PRIMARY explicitly reported it as unlinked.
 * Never derive this from a failed status load.
 */
export function NodeUnlinkedBanner({ unlinkedAt }: { unlinkedAt?: string }) {
  return (
    <div className="space-y-3 border-2 border-danger/60 bg-danger/5 p-4" role="alert" data-testid="node-unlinked-banner">
      <p className="pc-eyebrow text-danger">unlinked</p>
      <h3 className="pc-title text-xl text-mist">This NODE was unlinked by its PRIMARY</h3>
      <p className="text-sm leading-6 text-mute">
        It keeps serving its last configuration but no longer receives updates{unlinkedAt ? ` (unlinked at ${unlinkedAt})` : ""}.
        Link it to a PRIMARY again with a new enrollment token, or convert it to a PRIMARY.
      </p>
      <div className="flex flex-wrap gap-2">
        <button type="button" className="pc-btn" data-testid="node-unlinked-relink" onClick={() => focusControl("#node-enrollment-token")}>
          Re-link to a primary
        </button>
        <button type="button" className="pc-btn-ghost" data-testid="node-unlinked-convert" onClick={() => focusControl("#topology-promote-open")}>
          Convert to PRIMARY
        </button>
      </div>
    </div>
  );
}
