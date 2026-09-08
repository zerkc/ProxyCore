import { createPortal } from "react-dom";

export function IngressDialog(props: {
  open: boolean;
  initialIpv4: string;
  initialResolver: string;
  onClose: () => void;
  onSubmit: (payload: { ipv4: string; resolver: string }) => Promise<boolean>;
}) {
  if (!props.open) return null;

  let formRef: HTMLFormElement | undefined;

  return createPortal(
    <div
      className="fixed inset-0 z-50 grid place-items-center bg-bay/85 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="ingress-dialog-title"
    >
      <form
        ref={(el) => {
          formRef = el ?? undefined;
        }}
        className="pc-panel w-full max-w-md p-6 font-mono"
        onSubmit={(event) => {
          event.preventDefault();
          const fd = new FormData(event.currentTarget);
          void props.onSubmit({
            ipv4: (fd.get("ipv4") as string ?? "").trim(),
            resolver: (fd.get("resolver") as string ?? "").trim(),
          });
        }}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">ingress &amp; forwarding</p>
            <h2
              id="ingress-dialog-title"
              className="pc-title mt-2 text-2xl text-mist"
            >
              network settings
            </h2>
            <p className="mt-2 text-sm text-mute">
              Advertised IPv4 used in proxied DNS answers, and the default
              resolver for names outside your zones.
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
          Advertised IPv4
          <input
            name="ipv4"
            defaultValue={props.initialIpv4}
            className="pc-input"
            placeholder="Auto-detected LAN address"
          />
        </label>

        <label className="pc-label mt-4">
          Default resolver
          <input
            name="resolver"
            defaultValue={props.initialResolver}
            className="pc-input"
            placeholder="192.168.1.1"
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
            Save settings
          </button>
        </div>
      </form>
    </div>,
    document.body,
  );
}
