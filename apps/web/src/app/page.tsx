export default function HomePage() {
  return (
    <main className="mx-auto flex min-h-screen max-w-6xl flex-col gap-10 px-6 py-16">
      <header className="flex flex-col gap-4">
        <p className="text-sm uppercase tracking-[0.35em] text-emerald-300">
          ProxyCore / control plane
        </p>
        <h1 className="max-w-3xl text-5xl font-semibold tracking-tight">
          DNS that knows where your traffic belongs.
        </h1>
        <p className="max-w-2xl text-lg text-slate-300">
          A single-installation homelab control plane for authoritative local
          DNS, safe forwarding, and record-level ingress.
        </p>
      </header>
      <section className="grid gap-4 md:grid-cols-3" aria-label="System status">
        {[
          ["CoreDNS", "Ready to configure"],
          ["Nginx", "Awaiting first apply"],
          ["Worker", "Baseline online"],
        ].map(([service, status]) => (
          <article
            key={service}
            className="rounded-2xl border border-slate-700 bg-slate-900/70 p-5 shadow-xl shadow-slate-950/20"
          >
            <p className="text-sm text-slate-400">{service}</p>
            <p className="mt-3 text-xl font-medium text-emerald-200">{status}</p>
          </article>
        ))}
      </section>
      <p className="text-sm text-slate-400">
        Application baseline is ready. Bootstrap and configuration workflows
        will appear here as the control-plane modules land.
      </p>
    </main>
  );
}
