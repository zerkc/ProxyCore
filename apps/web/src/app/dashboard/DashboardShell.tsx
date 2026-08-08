"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";

type Zone = {
  id: string;
  name: string;
  records: Array<{
    id: string;
    name: string;
    type: string;
    value: unknown;
    proxied: boolean;
    enabled: boolean;
  }>;
};

type StatusPayload = {
  settings: {
    ingress: { ipv4?: string; ipv6?: string };
    defaultPool?: { id: string; endpoints: Array<{ host: string; port: number }> };
    forwardingRules: unknown[];
  };
  zones: Zone[];
  streams: unknown[];
  jobs: Array<{ id: string; status: string; target: string; createdAt: string }>;
  desiredRevision?: { revisionNumber: number; checksum: string };
  appliedRevision?: { revisionNumber: number; checksum: string };
};

const nav = [
  ["overview", "Pulse"],
  ["dns", "DNS & proxy"],
  ["network", "Streams"],
  ["operators", "Operators"],
] as const;

export default function DashboardShell() {
  const router = useRouter();
  const [view, setView] = useState<(typeof nav)[number][0]>("overview");
  const [status, setStatus] = useState<StatusPayload>();
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [zoneName, setZoneName] = useState("home.arpa");
  const [selectedZone, setSelectedZone] = useState("");
  const [recordName, setRecordName] = useState("gateway");
  const [recordType, setRecordType] = useState("A");
  const [recordValue, setRecordValue] = useState("192.168.1.20");
  const [proxied, setProxied] = useState(false);
  const [ingressIpv4, setIngressIpv4] = useState("");
  const [resolver, setResolver] = useState("192.168.1.1");

  const activeZone = useMemo(
    () => status?.zones.find((zone) => zone.id === selectedZone) ?? status?.zones[0],
    [selectedZone, status?.zones],
  );

  async function refresh() {
    const response = await fetch("/api/status", { cache: "no-store" });
    if (response.status === 401) {
      router.push("/login");
      return;
    }
    if (!response.ok) {
      setError("Status could not be loaded");
      return;
    }
    const payload = (await response.json()) as StatusPayload;
    setStatus(payload);
    setIngressIpv4(payload.settings.ingress.ipv4 ?? "");
    setSelectedZone((current) => current || payload.zones[0]?.id || "");
  }

  useEffect(() => {
    void refresh();
  }, []);

  async function createZone(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await mutate("/api/zones", { name: zoneName }, "Zone created");
  }

  async function createRecord(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!activeZone) {
      setError("Create a zone first");
      return;
    }
    await mutate(
      `/api/zones/${activeZone.id}/records`,
      {
        name: recordName,
        type: recordType,
        value: recordValue,
        enabled: true,
        proxied,
        proxy: proxied
          ? {
              origin: { ip: recordValue, port: 80, protocol: "http" },
              tlsEnabled: true,
            }
          : undefined,
      },
      "Record saved",
    );
  }

  async function saveNetwork(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await mutate(
      "/api/settings",
      {
        ingress: { ipv4: ingressIpv4 || undefined },
        defaultPool: {
          id: "default",
          endpoints: [{ host: resolver, port: 53 }],
        },
      },
      "Network settings saved",
      "PUT",
    );
  }

  async function apply() {
    await mutate("/api/apply", {}, "Apply queued");
  }

  async function mutate(path: string, body: unknown, success: string, method = "POST") {
    setMessage("");
    setError("");
    const response = await fetch(path, {
      method,
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    if (response.status === 401) {
      router.push("/login");
      return;
    }
    if (!response.ok) {
      setError((await response.json()).error ?? "Change rejected");
      return;
    }
    setMessage(success);
    await refresh();
  }

  async function logout() {
    await fetch("/api/auth/logout", { method: "POST" });
    router.push("/login");
  }

  return (
    <main className="min-h-screen">
      <div className="mx-auto grid min-h-screen max-w-[1500px] grid-cols-1 border-x border-slate-800 lg:grid-cols-[250px_1fr]">
        <aside className="border-b border-slate-800 bg-slate-950/50 p-6 lg:border-b-0 lg:border-r">
          <div className="flex items-center justify-between lg:block">
            <div>
              <p className="text-xs uppercase tracking-[0.32em] text-emerald-300">ProxyCore</p>
              <p className="mt-2 text-xs text-slate-500">Night-shift network desk</p>
            </div>
            <button className="text-xs text-slate-400 underline lg:mt-12" onClick={logout}>
              Sign out
            </button>
          </div>
          <nav className="mt-8 grid grid-cols-4 gap-2 lg:grid-cols-1" aria-label="Main navigation">
            {nav.map(([key, label]) => (
              <button
                key={key}
                onClick={() => setView(key)}
                className={`rounded-xl px-3 py-3 text-left text-sm transition ${
                  view === key
                    ? "bg-emerald-300 font-semibold text-slate-950"
                    : "text-slate-400 hover:bg-slate-900 hover:text-slate-100"
                }`}
              >
                {label}
              </button>
            ))}
          </nav>
          <div className="mt-12 hidden rounded-2xl border border-slate-800 bg-slate-900/60 p-4 lg:block">
            <p className="text-xs uppercase tracking-[0.2em] text-slate-500">Apply discipline</p>
            <p className="mt-3 text-sm leading-6 text-slate-300">
              Desired state is safe to edit. Nothing reaches the data plane until you publish it.
            </p>
          </div>
        </aside>

        <section className="p-6 md:p-10">
          <header className="flex flex-col justify-between gap-6 border-b border-slate-800 pb-8 md:flex-row md:items-end">
            <div>
              <p className="text-xs uppercase tracking-[0.32em] text-slate-500">Installation / local</p>
              <h1 className="mt-3 text-4xl font-semibold tracking-tight">
                {view === "overview" ? "Control room" : nav.find(([key]) => key === view)?.[1]}
              </h1>
              <p className="mt-3 max-w-2xl text-sm leading-6 text-slate-400">
                See what is intended, what is applied, and what still needs an operator&apos;s hand.
              </p>
            </div>
            <button
              onClick={apply}
              className="rounded-xl bg-emerald-300 px-5 py-3 text-sm font-semibold text-slate-950 transition hover:bg-emerald-200 focus:outline-none focus:ring-2 focus:ring-emerald-200"
            >
              Queue apply
            </button>
          </header>

          {message ? <p className="mt-6 rounded-xl border border-emerald-300/30 bg-emerald-300/10 p-3 text-sm text-emerald-100" role="status">{message}</p> : null}
          {error ? <p className="mt-6 rounded-xl border border-red-400/30 bg-red-400/10 p-3 text-sm text-red-200" role="alert">{error}</p> : null}

          {view === "overview" ? <Overview status={status} /> : null}
          {view === "dns" ? (
            <DnsView
              status={status}
              activeZone={activeZone}
              zoneName={zoneName}
              setZoneName={setZoneName}
              createZone={createZone}
              selectedZone={selectedZone}
              setSelectedZone={setSelectedZone}
              recordName={recordName}
              setRecordName={setRecordName}
              recordType={recordType}
              setRecordType={setRecordType}
              recordValue={recordValue}
              setRecordValue={setRecordValue}
              proxied={proxied}
              setProxied={setProxied}
              createRecord={createRecord}
            />
          ) : null}
          {view === "network" ? (
            <NetworkView
              ingressIpv4={ingressIpv4}
              setIngressIpv4={setIngressIpv4}
              resolver={resolver}
              setResolver={setResolver}
              saveNetwork={saveNetwork}
              streams={status?.streams ?? []}
            />
          ) : null}
          {view === "operators" ? <OperatorsView /> : null}
        </section>
      </div>
    </main>
  );
}

function Overview({ status }: { status?: StatusPayload }) {
  const cards = [
    ["CoreDNS", status?.zones.length ? `${status.zones.length} managed zones` : "No zones yet"],
    ["Nginx", status?.appliedRevision ? "Serving last applied revision" : "Awaiting first apply"],
    ["Worker", status?.jobs.some((job) => job.status === "queued") ? "Apply queued" : "Standing by"],
  ];
  return (
    <div className="mt-8 space-y-8">
      <div className="grid gap-4 md:grid-cols-3">
        {cards.map(([label, value]) => (
          <article key={label} className="rounded-2xl border border-slate-800 bg-slate-900/60 p-5">
            <p className="text-xs uppercase tracking-[0.22em] text-slate-500">{label}</p>
            <p className="mt-6 text-xl text-emerald-200">{value}</p>
          </article>
        ))}
      </div>
      <div className="rounded-2xl border border-slate-800 bg-slate-950/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">The safe path</p>
        <div className="mt-6 grid gap-3 text-sm md:grid-cols-5">
          {["Desired", "Revision", "Validate", "Promote", "Healthy"].map((step, index) => (
            <div key={step} className="flex items-center gap-3">
              <span className="grid size-8 place-items-center rounded-full border border-emerald-300/40 text-emerald-200">{index + 1}</span>
              <span className="text-slate-300">{step}</span>
            </div>
          ))}
        </div>
      </div>
      <div className="rounded-2xl border border-slate-800 bg-slate-900/40 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Revision ledger</p>
        <div className="mt-5 grid gap-4 md:grid-cols-2">
          <Revision label="Desired" revision={status?.desiredRevision} />
          <Revision label="Applied" revision={status?.appliedRevision} />
        </div>
      </div>
    </div>
  );
}

function Revision({ label, revision }: { label: string; revision?: { revisionNumber: number; checksum: string } }) {
  return (
    <div className="rounded-xl border border-slate-800 bg-slate-950/60 p-4">
      <p className="text-xs uppercase tracking-[0.2em] text-slate-500">{label}</p>
      <p className="mt-3 text-lg text-slate-200">{revision ? `Revision ${revision.revisionNumber}` : "Nothing recorded"}</p>
      <p className="mt-2 truncate font-mono text-xs text-slate-500">{revision?.checksum ?? "—"}</p>
    </div>
  );
}

function DnsView(props: {
  status?: StatusPayload;
  activeZone?: Zone;
  zoneName: string;
  setZoneName: (value: string) => void;
  createZone: (event: FormEvent<HTMLFormElement>) => void;
  selectedZone: string;
  setSelectedZone: (value: string) => void;
  recordName: string;
  setRecordName: (value: string) => void;
  recordType: string;
  setRecordType: (value: string) => void;
  recordValue: string;
  setRecordValue: (value: string) => void;
  proxied: boolean;
  setProxied: (value: boolean) => void;
  createRecord: (event: FormEvent<HTMLFormElement>) => void;
}) {
  return (
    <div className="mt-8 grid gap-6 xl:grid-cols-[1fr_1.2fr]">
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Managed zones</p>
        <form className="mt-5 flex gap-2" onSubmit={props.createZone}>
          <input value={props.zoneName} onChange={(event) => props.setZoneName(event.target.value)} className="min-w-0 flex-1 rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 text-sm outline-none focus:border-emerald-300" aria-label="Zone name" />
          <button className="rounded-xl border border-emerald-300/40 px-3 py-2 text-sm text-emerald-200 hover:bg-emerald-300/10">Add zone</button>
        </form>
        <div className="mt-5 space-y-2">
          {props.status?.zones.length ? props.status.zones.map((zone) => (
            <button key={zone.id} onClick={() => props.setSelectedZone(zone.id)} className={`flex w-full items-center justify-between rounded-xl px-4 py-3 text-left text-sm ${props.activeZone?.id === zone.id ? "bg-emerald-300/15 text-emerald-100" : "bg-slate-950/50 text-slate-400"}`}>
              <span>{zone.name}</span><span className="font-mono text-xs">{zone.records.length} records</span>
            </button>
          )) : <p className="rounded-xl border border-dashed border-slate-700 p-4 text-sm text-slate-500">No zones yet. Add the namespace your homelab owns.</p>}
        </div>
      </section>
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Records / {props.activeZone?.name ?? "select a zone"}</p>
        <form className="mt-5 grid gap-3 md:grid-cols-2" onSubmit={props.createRecord}>
          <input value={props.recordName} onChange={(event) => props.setRecordName(event.target.value)} className="rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 text-sm outline-none focus:border-emerald-300" placeholder="Record name" aria-label="Record name" />
          <select value={props.recordType} onChange={(event) => props.setRecordType(event.target.value)} className="rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 text-sm outline-none focus:border-emerald-300" aria-label="Record type">
            {["A", "AAAA", "CNAME", "TXT", "MX", "SRV"].map((type) => <option key={type}>{type}</option>)}
          </select>
          <input value={props.recordValue} onChange={(event) => props.setRecordValue(event.target.value)} className="rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 text-sm outline-none focus:border-emerald-300 md:col-span-2" placeholder="Value" aria-label="Record value" />
          <label className="flex items-center gap-3 text-sm text-slate-300 md:col-span-2"><input type="checkbox" checked={props.proxied} onChange={(event) => props.setProxied(event.target.checked)} className="size-4 accent-emerald-300" /> Proxy through ProxyCore</label>
          <button className="rounded-xl bg-slate-100 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-white md:col-span-2" type="submit">Save record</button>
        </form>
        <div className="mt-7 space-y-2">
          {props.activeZone?.records.map((record) => <div key={record.id} className="flex items-center justify-between rounded-xl border border-slate-800 bg-slate-950/60 px-4 py-3 text-sm"><span className="text-slate-200">{record.name}</span><span className="font-mono text-xs text-slate-500">{record.type} / {record.proxied ? "proxied" : "DNS-only"}</span></div>)}
        </div>
      </section>
    </div>
  );
}

function NetworkView(props: { ingressIpv4: string; setIngressIpv4: (value: string) => void; resolver: string; setResolver: (value: string) => void; saveNetwork: (event: FormEvent<HTMLFormElement>) => void; streams: unknown[] }) {
  return (
    <div className="mt-8 grid gap-6 xl:grid-cols-[1fr_1fr]">
      <form onSubmit={props.saveNetwork} className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Ingress & forwarding</p>
        <div className="mt-5 space-y-4">
          <label className="block text-sm text-slate-300">Proxy ingress IPv4<input value={props.ingressIpv4} onChange={(event) => props.setIngressIpv4(event.target.value)} className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 outline-none focus:border-emerald-300" placeholder="192.168.1.10" /></label>
          <label className="block text-sm text-slate-300">Default resolver<input value={props.resolver} onChange={(event) => props.setResolver(event.target.value)} className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 outline-none focus:border-emerald-300" placeholder="192.168.1.1" /></label>
        </div>
        <button className="mt-6 rounded-xl bg-emerald-300 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-emerald-200" type="submit">Save network settings</button>
      </form>
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Streams</p>
        <p className="mt-5 text-sm leading-6 text-slate-400">{props.streams.length ? `${props.streams.length} explicit listener(s) configured.` : "No TCP/UDP listeners yet. Streams stay separate from HTTP proxy records."}</p>
      </section>
    </div>
  );
}

function OperatorsView() {
  return (
    <section className="mt-8 rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
      <p className="text-xs uppercase tracking-[0.22em] text-slate-500">Local identities</p>
      <h2 className="mt-4 text-2xl font-medium">Owner / Operator access</h2>
      <p className="mt-3 max-w-xl text-sm leading-6 text-slate-400">User management is intentionally server-authorized and audit-backed. Use the API endpoint to create the first Operator while the dedicated table view is being expanded.</p>
      <code className="mt-6 block rounded-xl bg-slate-950 p-4 text-xs text-emerald-200">POST /api/users</code>
    </section>
  );
}
