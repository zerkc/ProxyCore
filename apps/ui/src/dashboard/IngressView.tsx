import { useState } from "react";
import { useDashboard } from "./dashboard-context";
import { IngressDialog } from "./IngressDialog";

export function IngressView() {
  const {
    ingressIpv4: initialIpv4,
    resolver: initialResolver,
    saveNetwork,
  } = useDashboard();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [ipv4, setIpv4] = useState(initialIpv4);
  const [resolver, setResolver] = useState(initialResolver);

  return (
    <div className="mt-8 max-w-xl">
      <div className="pc-panel p-6 md:p-8">
        <p className="pc-eyebrow">Ingress & forwarding</p>
        <h2 className="pc-title mt-2 text-2xl text-mist">
          Network configuration
        </h2>
        <p className="mt-3 text-sm leading-6 text-mute">
          Advertised IPv4 used in proxied DNS answers, and the default resolver
          for names outside your zones.
        </p>
        <button
          type="button"
          className="pc-btn mt-6"
          onClick={() => {
            setIpv4(initialIpv4);
            setResolver(initialResolver);
            setDialogOpen(true);
          }}
        >
          Configure network settings
        </button>
      </div>

      <IngressDialog
        open={dialogOpen}
        initialIpv4={initialIpv4}
        initialResolver={initialResolver}
        onClose={() => setDialogOpen(false)}
        onSubmit={async ({ ipv4: newIpv4, resolver: newResolver }) => {
          const ok = await saveNetwork(newIpv4, newResolver);
          if (ok) {
            setDialogOpen(false);
          }
          return ok;
        }}
      />
    </div>
  );
}
