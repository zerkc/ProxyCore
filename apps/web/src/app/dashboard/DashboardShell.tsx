"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { RecordDialog, type EditableRecord } from "./RecordDialog";

type Zone = {
  id: string;
  name: string;
  records: EditableRecord[];
};

type StatusPayload = {
  settings: {
    ingress: { ipv4?: string; ipv6?: string };
    defaultPool?: {
      id: string;
      endpoints: Array<{ host: string; port: number }>;
    };
    forwardingRules: unknown[];
  };
  zones: Zone[];
  streams: unknown[];
  jobs: Array<{
    id: string;
    status: string;
    target: string;
    createdAt: string;
  }>;
  certificates: Array<{
    id: string;
    hostnames: string[];
    issuer: string;
    status: string;
  }>;
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
  const [recordDialogOpen, setRecordDialogOpen] = useState(false);
  const [editingRecord, setEditingRecord] = useState<EditableRecord>();
  const [ingressIpv4, setIngressIpv4] = useState("");
  const [resolver, setResolver] = useState("192.168.1.1");
  const [certificateHostnames, setCertificateHostnames] =
    useState("app.home.arpa");

  const activeZone = useMemo(
    () =>
      status?.zones.find((zone) => zone.id === selectedZone) ??
      status?.zones[0],
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

  async function saveRecord(
    payload: Record<string, unknown>,
  ): Promise<boolean> {
    if (!activeZone) {
      setError("Create a zone first");
      return false;
    }
    const path = editingRecord
      ? `/api/zones/${activeZone.id}/records/${editingRecord.id}`
      : `/api/zones/${activeZone.id}/records`;
    const saved = await mutate(
      path,
      payload,
      editingRecord ? "Record updated" : "Record saved",
      editingRecord ? "PATCH" : "POST",
    );
    if (saved) {
      setRecordDialogOpen(false);
      setEditingRecord(undefined);
    }
    return saved;
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

  async function issueCertificate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const hostnames = certificateHostnames
      .split(",")
      .map((hostname) => hostname.trim())
      .filter(Boolean);
    if (hostnames.length === 0) {
      setError("Enter at least one certificate hostname");
      return;
    }
    await mutate(
      "/api/certificates",
      {
        hostnames,
        issuer: "self-signed",
        challenge: "none",
      },
      "Self-signed certificate issued",
    );
  }

  async function apply() {
    await mutate("/api/apply", {}, "Apply queued");
  }

  async function mutate(
    path: string,
    body: unknown,
    success: string,
    method = "POST",
  ): Promise<boolean> {
    setMessage("");
    setError("");
    const response = await fetch(path, {
      method,
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    if (response.status === 401) {
      router.push("/login");
      return false;
    }
    if (!response.ok) {
      setError((await response.json()).error ?? "Change rejected");
      return false;
    }
    setMessage(success);
    await refresh();
    return true;
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
              <p className="text-xs uppercase tracking-[0.32em] text-emerald-300">
                ProxyCore
              </p>
              <p className="mt-2 text-xs text-slate-500">
                Night-shift network desk
              </p>
            </div>
            <button
              className="text-xs text-slate-400 underline lg:mt-12"
              onClick={logout}
            >
              Sign out
            </button>
          </div>
          <nav
            className="mt-8 grid grid-cols-4 gap-2 lg:grid-cols-1"
            aria-label="Main navigation"
          >
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
            <p className="text-xs uppercase tracking-[0.2em] text-slate-500">
              Apply discipline
            </p>
            <p className="mt-3 text-sm leading-6 text-slate-300">
              Saving a zone or record queues an apply immediately. Use the
              manual action only to re-apply the current desired state.
            </p>
          </div>
        </aside>

        <section className="p-6 md:p-10">
          <header className="flex flex-col justify-between gap-6 border-b border-slate-800 pb-8 md:flex-row md:items-end">
            <div>
              <p className="text-xs uppercase tracking-[0.32em] text-slate-500">
                Installation / local
              </p>
              <h1 className="mt-3 text-4xl font-semibold tracking-tight">
                {view === "overview"
                  ? "Control room"
                  : nav.find(([key]) => key === view)?.[1]}
              </h1>
              <p className="mt-3 max-w-2xl text-sm leading-6 text-slate-400">
                See what is intended, what is applied, and what still needs an
                operator&apos;s hand.
              </p>
            </div>
            <button
              onClick={apply}
              className="rounded-xl bg-emerald-300 px-5 py-3 text-sm font-semibold text-slate-950 transition hover:bg-emerald-200 focus:outline-none focus:ring-2 focus:ring-emerald-200"
            >
              Re-apply current state
            </button>
          </header>

          {message ? (
            <p
              className="mt-6 rounded-xl border border-emerald-300/30 bg-emerald-300/10 p-3 text-sm text-emerald-100"
              role="status"
            >
              {message}
            </p>
          ) : null}
          {error ? (
            <p
              className="mt-6 rounded-xl border border-red-400/30 bg-red-400/10 p-3 text-sm text-red-200"
              role="alert"
            >
              {error}
            </p>
          ) : null}

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
              openCreate={() => {
                setEditingRecord(undefined);
                setRecordDialogOpen(true);
              }}
              openEdit={(record) => {
                setEditingRecord(record);
                setRecordDialogOpen(true);
              }}
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
              certificates={status?.certificates ?? []}
              certificateHostnames={certificateHostnames}
              setCertificateHostnames={setCertificateHostnames}
              issueCertificate={issueCertificate}
            />
          ) : null}
          {view === "operators" ? <OperatorsView /> : null}
        </section>
      </div>

      <RecordDialog
        open={recordDialogOpen}
        zoneName={activeZone?.name}
        certificates={status?.certificates ?? []}
        initial={editingRecord}
        onClose={() => {
          setRecordDialogOpen(false);
          setEditingRecord(undefined);
        }}
        onSubmit={saveRecord}
      />
    </main>
  );
}

function Overview({ status }: { status?: StatusPayload }) {
  const cards = [
    [
      "CoreDNS",
      status?.zones.length
        ? `${status.zones.length} managed zones`
        : "No zones yet",
    ],
    [
      "Nginx",
      status?.appliedRevision
        ? "Serving last applied revision"
        : "Awaiting first apply",
    ],
    [
      "Worker",
      status?.jobs.some((job) => job.status === "queued")
        ? "Apply queued"
        : "Standing by",
    ],
  ];
  return (
    <div className="mt-8 space-y-8">
      <div className="grid gap-4 md:grid-cols-3">
        {cards.map(([label, value]) => (
          <article
            key={label}
            className="rounded-2xl border border-slate-800 bg-slate-900/60 p-5"
          >
            <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
              {label}
            </p>
            <p className="mt-6 text-xl text-emerald-200">{value}</p>
          </article>
        ))}
      </div>
      <div className="rounded-2xl border border-slate-800 bg-slate-950/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          The safe path
        </p>
        <div className="mt-6 grid gap-3 text-sm md:grid-cols-5">
          {["Desired", "Revision", "Validate", "Promote", "Healthy"].map(
            (step, index) => (
              <div key={step} className="flex items-center gap-3">
                <span className="grid size-8 place-items-center rounded-full border border-emerald-300/40 text-emerald-200">
                  {index + 1}
                </span>
                <span className="text-slate-300">{step}</span>
              </div>
            ),
          )}
        </div>
      </div>
      <div className="rounded-2xl border border-slate-800 bg-slate-900/40 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Revision ledger
        </p>
        <div className="mt-5 grid gap-4 md:grid-cols-2">
          <Revision label="Desired" revision={status?.desiredRevision} />
          <Revision label="Applied" revision={status?.appliedRevision} />
        </div>
      </div>
    </div>
  );
}

function Revision({
  label,
  revision,
}: {
  label: string;
  revision?: { revisionNumber: number; checksum: string };
}) {
  return (
    <div className="rounded-xl border border-slate-800 bg-slate-950/60 p-4">
      <p className="text-xs uppercase tracking-[0.2em] text-slate-500">
        {label}
      </p>
      <p className="mt-3 text-lg text-slate-200">
        {revision ? `Revision ${revision.revisionNumber}` : "Nothing recorded"}
      </p>
      <p className="mt-2 truncate font-mono text-xs text-slate-500">
        {revision?.checksum ?? "—"}
      </p>
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
  openCreate: () => void;
  openEdit: (record: EditableRecord) => void;
}) {
  return (
    <div className="mt-8 grid gap-6 xl:grid-cols-[1fr_1.2fr]">
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Managed zones
        </p>
        <form className="mt-5 flex gap-2" onSubmit={props.createZone}>
          <input
            value={props.zoneName}
            onChange={(event) => props.setZoneName(event.target.value)}
            className="min-w-0 flex-1 rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 text-sm outline-none focus:border-emerald-300"
            aria-label="Zone name"
          />
          <button className="rounded-xl border border-emerald-300/40 px-3 py-2 text-sm text-emerald-200 hover:bg-emerald-300/10">
            Add zone
          </button>
        </form>
        <div className="mt-5 space-y-2">
          {props.status?.zones.length ? (
            props.status.zones.map((zone) => (
              <button
                key={zone.id}
                onClick={() => props.setSelectedZone(zone.id)}
                className={`flex w-full items-center justify-between rounded-xl px-4 py-3 text-left text-sm ${
                  props.activeZone?.id === zone.id
                    ? "bg-emerald-300/15 text-emerald-100"
                    : "bg-slate-950/50 text-slate-400"
                }`}
              >
                <span>{zone.name}</span>
                <span className="font-mono text-xs">
                  {zone.records.length} records
                </span>
              </button>
            ))
          ) : (
            <p className="rounded-xl border border-dashed border-slate-700 p-4 text-sm text-slate-500">
              No zones yet. Add the namespace your homelab owns.
            </p>
          )}
        </div>
      </section>
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Records / {props.activeZone?.name ?? "select a zone"}
        </p>
        <button
          className="mt-5 rounded-xl bg-slate-100 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-white disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          disabled={!props.activeZone}
          onClick={props.openCreate}
        >
          Add DNS record
        </button>
        <div className="mt-7 space-y-2">
          {props.activeZone?.records.map((record) => (
            <div
              key={record.id}
              className="flex items-center justify-between gap-3 rounded-xl border border-slate-800 bg-slate-950/60 px-4 py-3 text-sm"
            >
              <div className="min-w-0">
                <p className="truncate text-slate-200">{record.name}</p>
                <p className="mt-1 font-mono text-xs text-slate-500">
                  {record.type} /{" "}
                  {record.proxied ? summarizeProxy(record) : "DNS-only"}
                </p>
              </div>
              <button
                type="button"
                className="shrink-0 rounded-lg border border-slate-700 px-3 py-1.5 text-xs text-emerald-200 hover:bg-emerald-300/10"
                onClick={() => props.openEdit(record)}
              >
                Configure
              </button>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function summarizeProxy(record: EditableRecord): string {
  const origin = record.proxy?.origin;
  if (!origin?.ip || !origin.port) return "proxied";
  return `proxied → ${origin.ip}:${origin.port}`;
}

function NetworkView(props: {
  ingressIpv4: string;
  setIngressIpv4: (value: string) => void;
  resolver: string;
  setResolver: (value: string) => void;
  saveNetwork: (event: FormEvent<HTMLFormElement>) => void;
  streams: unknown[];
  certificates: Array<{
    id: string;
    hostnames: string[];
    issuer: string;
    status: string;
  }>;
  certificateHostnames: string;
  setCertificateHostnames: (value: string) => void;
  issueCertificate: (event: FormEvent<HTMLFormElement>) => void;
}) {
  return (
    <div className="mt-8 grid gap-6 xl:grid-cols-[1fr_1fr]">
      <form
        onSubmit={props.saveNetwork}
        className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6"
      >
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Ingress & forwarding
        </p>
        <div className="mt-5 space-y-4">
          <label className="block text-sm text-slate-300">
            Proxy advertised IPv4
            <input
              value={props.ingressIpv4}
              onChange={(event) => props.setIngressIpv4(event.target.value)}
              className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 outline-none focus:border-emerald-300"
              placeholder="Auto-detected LAN address"
            />
          </label>
          <p className="-mt-2 text-xs leading-5 text-slate-500">
            Used in proxied DNS answers. It is detected automatically when
            possible and can be overridden for another interface, NAT, or public
            address.
          </p>
          <label className="block text-sm text-slate-300">
            Default resolver
            <input
              value={props.resolver}
              onChange={(event) => props.setResolver(event.target.value)}
              className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 outline-none focus:border-emerald-300"
              placeholder="192.168.1.1"
            />
          </label>
        </div>
        <button
          className="mt-6 rounded-xl bg-emerald-300 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-emerald-200"
          type="submit"
        >
          Save network settings
        </button>
      </form>
      <form
        onSubmit={props.issueCertificate}
        className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6"
      >
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Certificates
        </p>
        <p className="mt-3 text-sm leading-6 text-slate-400">
          Issue a self-signed certificate for HTTPS proxied records. Use the
          Compose demo origin at{" "}
          <code className="text-emerald-200">172.30.0.10:80</code>.
        </p>
        <label className="mt-5 block text-sm text-slate-300">
          Hostnames
          <input
            value={props.certificateHostnames}
            onChange={(event) =>
              props.setCertificateHostnames(event.target.value)
            }
            className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-950 px-3 py-2 outline-none focus:border-emerald-300"
            placeholder="app.home.arpa, *.home.arpa"
          />
        </label>
        <button
          className="mt-6 rounded-xl bg-emerald-300 px-4 py-2 text-sm font-semibold text-slate-950 hover:bg-emerald-200"
          type="submit"
        >
          Issue self-signed certificate
        </button>
        <div className="mt-5 space-y-2">
          {props.certificates.length ? (
            props.certificates.map((certificate) => (
              <div
                key={certificate.id}
                className="rounded-xl border border-slate-800 bg-slate-950/60 px-4 py-3 text-sm"
              >
                <p className="text-slate-200">
                  {certificate.hostnames.join(", ")}
                </p>
                <p className="mt-1 font-mono text-xs text-slate-500">
                  {certificate.issuer} / {certificate.status}
                </p>
              </div>
            ))
          ) : (
            <p className="text-sm text-slate-500">
              No certificates issued yet.
            </p>
          )}
        </div>
      </form>
      <section className="rounded-2xl border border-slate-800 bg-slate-900/50 p-6 xl:col-span-2">
        <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
          Streams
        </p>
        <p className="mt-5 text-sm leading-6 text-slate-400">
          {props.streams.length
            ? `${props.streams.length} explicit listener(s) configured.`
            : "No TCP/UDP listeners yet. Streams stay separate from HTTP proxy records."}
        </p>
      </section>
    </div>
  );
}

function OperatorsView() {
  return (
    <section className="mt-8 rounded-2xl border border-slate-800 bg-slate-900/50 p-6">
      <p className="text-xs uppercase tracking-[0.22em] text-slate-500">
        Local identities
      </p>
      <h2 className="mt-4 text-2xl font-medium">Owner / Operator access</h2>
      <p className="mt-3 max-w-xl text-sm leading-6 text-slate-400">
        User management is intentionally server-authorized and audit-backed. Use
        the API endpoint to create the first Operator while the dedicated table
        view is being expanded.
      </p>
      <code className="mt-6 block rounded-xl bg-slate-950 p-4 text-xs text-emerald-200">
        POST /api/users
      </code>
    </section>
  );
}
