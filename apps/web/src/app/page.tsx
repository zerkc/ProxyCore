export default function HomePage() {
  return (
    <main className="relative mx-auto flex min-h-screen max-w-6xl flex-col justify-center gap-10 px-6 py-16">
      <header className="pc-enter flex flex-col gap-5">
        <p className="pc-title text-5xl text-mist md:text-6xl">ProxyCore</p>
        <h1 className="max-w-2xl text-xl font-medium tracking-tight text-mist/90 md:text-2xl">
          DNS that knows where your traffic belongs.
        </h1>
        <p className="max-w-xl text-base leading-7 text-mute">
          A single-installation homelab control plane for authoritative local
          DNS, safe forwarding, and record-level ingress.
        </p>
        <div className="mt-2 flex flex-wrap gap-3">
          <a href="/login" className="pc-btn">
            Open control room
          </a>
          <a href="/bootstrap" className="pc-btn-ghost">
            Bootstrap Owner
          </a>
        </div>
      </header>
      <section
        className="pc-enter grid gap-3 md:grid-cols-3"
        aria-label="System status"
        style={{ animationDelay: "80ms" }}
      >
        {[
          ["CoreDNS", "Ready to configure"],
          ["Nginx", "Awaiting first apply"],
          ["Worker", "Baseline online"],
        ].map(([service, status]) => (
          <article key={service} className="pc-panel p-5">
            <p className="pc-eyebrow">{service}</p>
            <p className="mt-4 font-mono text-lg text-link">{status}</p>
          </article>
        ))}
      </section>
    </main>
  );
}
