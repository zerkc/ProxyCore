import { createPortal } from "react-dom";

export function ZoneDialog(props: {
  open: boolean;
  onClose: () => void;
  onSubmit: (name: string) => Promise<boolean>;
}) {
  if (!props.open) return null;

  return createPortal(
    <div
      className="fixed inset-0 z-50 grid place-items-center bg-bay/85 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="zone-dialog-title"
    >
      <form
        className="pc-panel w-full max-w-md p-6 shadow-2xl shadow-black/40"
        onSubmit={(event) => {
          event.preventDefault();
          const form = event.currentTarget;
          const name = (form.elements.namedItem("name") as HTMLInputElement).value.trim();
          if (!name) return;
          void props.onSubmit(name);
        }}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">DNS namespace</p>
            <h2
              id="zone-dialog-title"
              className="pc-title mt-2 text-2xl text-mist"
            >
              Add a new zone
            </h2>
            <p className="mt-2 text-sm text-mute">
              The namespace this homelab owns authoritatively. You can open it
              right after to add records.
            </p>
          </div>
          <button
            type="button"
            className="text-sm text-mute transition hover:text-mist"
            onClick={props.onClose}
          >
            Close
          </button>
        </div>

        <label className="pc-label mt-6">
          Zone name
          <input
            name="name"
            className="pc-input"
            placeholder="home.arpa"
            required
            autoFocus
          />
        </label>

        <div className="mt-6 flex justify-end gap-3">
          <button
            type="button"
            className="pc-btn-ghost"
            onClick={props.onClose}
          >
            Cancel
          </button>
          <button className="pc-btn" type="submit">
            Add zone
          </button>
        </div>
      </form>
    </div>,
    document.body,
  );
}
