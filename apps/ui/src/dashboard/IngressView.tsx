import { useDashboard } from "./dashboard-context";

export function IngressView() {
  const {
    ingressIpv4,
    setIngressIpv4,
    resolver,
    setResolver,
    saveNetwork,
  } = useDashboard();

  return (
    <div className="mt-8 max-w-xl">
      <form onSubmit={saveNetwork} className="pc-panel p-6 md:p-8">
        <p className="pc-eyebrow">Ingress & forwarding</p>
        <h2 className="pc-title mt-2 text-2xl text-mist">
          How ProxyCore answers and forwards
        </h2>
        <p className="mt-3 text-sm leading-6 text-mute">
          These settings affect proxied DNS answers and the default resolver
          used for names outside your zones. TCP/UDP port maps live under
          Streams.
        </p>
        <div className="mt-6 space-y-4">
          <label className="pc-label">
            Proxy advertised IPv4
            <input
              value={ingressIpv4}
              onChange={(event) => setIngressIpv4(event.target.value)}
              className="pc-input"
              placeholder="Auto-detected LAN address"
            />
          </label>
          <p className="-mt-2 text-xs leading-5 text-faint">
            Used in proxied DNS answers. Detected automatically when possible;
            override for another interface, NAT, or public address.
          </p>
          <label className="pc-label">
            Default resolver
            <input
              value={resolver}
              onChange={(event) => setResolver(event.target.value)}
              className="pc-input"
              placeholder="192.168.1.1"
            />
          </label>
        </div>
        <button className="pc-btn mt-6" type="submit">
          Save network settings
        </button>
      </form>
    </div>
  );
}
