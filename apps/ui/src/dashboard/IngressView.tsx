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
    <div className="mt-8">
      <header className="flex flex-col gap-5 border-b border-line/80 pb-5 md:flex-row md:items-end md:justify-between">
        <div className="min-w-0">
          <p className="pc-eyebrow">ingress</p>
          <h2 className="pc-title mt-2 text-2xl text-mist">
            Network configuration
          </h2>
          <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
            Advertised IPv4 used in proxied DNS answers, and the default resolver
            for names outside your zones.
          </p>
        </div>
        <button
          type="button"
          className="pc-btn shrink-0 self-start md:self-auto"
          onClick={() => {
            setIpv4(initialIpv4);
            setResolver(initialResolver);
            setDialogOpen(true);
          }}
        >
          Configure network settings
        </button>
      </header>

      <dl className="grid border-b border-line/80 sm:grid-cols-2">
        <div className="border-b border-line/80 py-4 sm:border-b-0 sm:border-r sm:pr-6">
          <dt className="font-mono text-[11px] uppercase tracking-[0.1em] text-faint">
            Advertised IPv4
          </dt>
          <dd className="mt-2 break-all font-mono text-sm text-link">
            {initialIpv4 || "—"}
          </dd>
        </div>
        <div className="py-4 sm:pl-6">
          <dt className="font-mono text-[11px] uppercase tracking-[0.1em] text-faint">
            Default resolver
          </dt>
          <dd className="mt-2 break-all font-mono text-sm text-link">
            {initialResolver || "—"}
          </dd>
        </div>
      </dl>

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
