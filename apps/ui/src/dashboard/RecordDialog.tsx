import { FormEvent, useEffect, useMemo, useState } from "react";

export type CertificateOption = {
  id: string;
  hostnames: string[];
  issuer: string;
  status: string;
};

export type EditableRecord = {
  id: string;
  name: string;
  type: string;
  value: unknown;
  proxied: boolean;
  enabled: boolean;
  proxy?: {
    origin?: { ip?: string; port?: number; protocol?: string };
    tlsEnabled?: boolean;
    redirectHttpToHttps?: boolean;
    certificateId?: string;
    http2?: boolean;
    http3?: boolean;
    nginxDirectives?: string;
    headers?: Array<{ name: string; value: string }>;
    pathRules?: Array<{
      kind: "exact" | "prefix";
      pattern: string;
      action:
        | { type: "proxy"; rewrite?: string }
        | {
            type: "redirect";
            status: 301 | 302 | 307 | 308;
            location?: string;
          };
    }>;
    basicAuth?: { username: string; passwordSecretId: string };
    websocket?: boolean;
    cache?: { enabled: boolean };
    backendTlsVerify?: boolean;
    timeouts?: {
      connectSeconds?: number;
      sendReadSeconds?: number;
      clientHeaderSeconds?: number;
      bodyLimitMb?: number;
    };
  };
};

type PathRuleDraft = {
  kind: "exact" | "prefix";
  pattern: string;
  actionType: "proxy" | "redirect";
  status: 301 | 302 | 307 | 308;
  location: string;
  rewrite: string;
};

type ProxyTab = "record" | "origin" | "tls" | "nginx" | "routes" | "access";

const proxyTabs: Array<{ id: ProxyTab; label: string; hint: string }> = [
  { id: "record", label: "Record", hint: "DNS identity" },
  { id: "origin", label: "Origin", hint: "Upstream target" },
  { id: "tls", label: "TLS", hint: "Client access" },
  { id: "nginx", label: "Nginx", hint: "Server directives" },
  { id: "routes", label: "Routes", hint: "Paths and redirects" },
  { id: "access", label: "Access", hint: "Basic Auth" },
];

const inputClass = "pc-input";
const labelClass = "pc-label";
const sectionClass = "pc-panel-quiet p-4";

export function RecordDialog(props: {
  open: boolean;
  zoneName?: string;
  certificates: CertificateOption[];
  initial?: EditableRecord;
  onClose: () => void;
  onSubmit: (payload: Record<string, unknown>) => Promise<boolean>;
}) {
  const editing = Boolean(props.initial);
  const [recordName, setRecordName] = useState("gateway");
  const [recordType, setRecordType] = useState("A");
  const [recordValue, setRecordValue] = useState("192.168.1.20");
  const [proxied, setProxied] = useState(false);
  const [originIp, setOriginIp] = useState("192.168.1.20");
  const [originPort, setOriginPort] = useState(80);
  const [originProtocol, setOriginProtocol] = useState<"http" | "https">(
    "http",
  );
  const [proxyTlsEnabled, setProxyTlsEnabled] = useState(true);
  const [proxyCertificateId, setProxyCertificateId] = useState("");
  const [redirectHttpToHttps, setRedirectHttpToHttps] = useState(true);
  const [http2, setHttp2] = useState(true);
  const [http3, setHttp3] = useState(false);
  const [websocket, setWebsocket] = useState(false);
  const [cacheEnabled, setCacheEnabled] = useState(false);
  const [backendTlsVerify, setBackendTlsVerify] = useState(false);
  const [connectSeconds, setConnectSeconds] = useState(5);
  const [sendReadSeconds, setSendReadSeconds] = useState(60);
  const [clientHeaderSeconds, setClientHeaderSeconds] = useState(15);
  const [bodyLimitMb, setBodyLimitMb] = useState(10);
  const [nginxDirectives, setNginxDirectives] = useState("");
  const [pathRules, setPathRules] = useState<PathRuleDraft[]>([]);
  const [basicAuthEnabled, setBasicAuthEnabled] = useState(false);
  const [basicAuthUsername, setBasicAuthUsername] = useState("");
  const [basicAuthPassword, setBasicAuthPassword] = useState("");
  const [existingSecretId, setExistingSecretId] = useState<string>();
  const [localError, setLocalError] = useState("");
  const [activeTab, setActiveTab] = useState<ProxyTab>("record");

  const availableCertificates = useMemo(
    () =>
      props.certificates.filter(
        (certificate) =>
          (certificate.status === "active" ||
            certificate.status === "issued") &&
          certificateCoversHostname(
            certificate.hostnames,
            canonicalRecordName(recordName, props.zoneName),
          ),
      ),
    [props.certificates, props.zoneName, recordName],
  );

  useEffect(() => {
    if (!props.open) return;
    const initial = props.initial;
    const value =
      typeof initial?.value === "string"
        ? initial.value
        : typeof initial?.value === "object" && initial?.value
          ? JSON.stringify(initial.value)
          : "192.168.1.20";
    const type = initial?.type ?? "A";
    setRecordName(
      initial ? shortName(initial.name, props.zoneName) : "gateway",
    );
    setRecordType(type);
    setRecordValue(value);
    setProxied(initial?.proxied ?? false);
    const proxy = initial?.proxy;
    const defaultOrigin =
      type === "A" || type === "AAAA" ? value : (proxy?.origin?.ip ?? "");
    setOriginIp(proxy?.origin?.ip ?? defaultOrigin);
    setOriginPort(proxy?.origin?.port ?? 80);
    setOriginProtocol(proxy?.origin?.protocol === "https" ? "https" : "http");
    setProxyTlsEnabled(proxy?.tlsEnabled ?? true);
    setProxyCertificateId(proxy?.certificateId ?? "");
    setRedirectHttpToHttps(
      proxy?.redirectHttpToHttps ?? proxy?.tlsEnabled ?? true,
    );
    setHttp2(proxy?.http2 ?? proxy?.tlsEnabled ?? true);
    setHttp3(proxy?.http3 ?? false);
    setWebsocket(proxy?.websocket ?? false);
    setCacheEnabled(proxy?.cache?.enabled ?? false);
    setBackendTlsVerify(proxy?.backendTlsVerify ?? false);
    setConnectSeconds(proxy?.timeouts?.connectSeconds ?? 5);
    setSendReadSeconds(proxy?.timeouts?.sendReadSeconds ?? 60);
    setClientHeaderSeconds(proxy?.timeouts?.clientHeaderSeconds ?? 15);
    setBodyLimitMb(proxy?.timeouts?.bodyLimitMb ?? 10);
    setNginxDirectives(
      proxy?.nginxDirectives ?? formatHeaderDirectives(proxy?.headers),
    );
    setPathRules(
      proxy?.pathRules?.map((rule) => ({
        kind: rule.kind,
        pattern: rule.pattern,
        actionType: rule.action.type,
        status: rule.action.type === "redirect" ? rule.action.status : 307,
        location:
          rule.action.type === "redirect" ? (rule.action.location ?? "/") : "/",
        rewrite:
          rule.action.type === "proxy" ? (rule.action.rewrite ?? "") : "",
      })) ?? [],
    );
    setBasicAuthEnabled(Boolean(proxy?.basicAuth));
    setBasicAuthUsername(proxy?.basicAuth?.username ?? "");
    setBasicAuthPassword("");
    setExistingSecretId(proxy?.basicAuth?.passwordSecretId);
    setLocalError("");
    setActiveTab("record");
  }, [props.open, props.initial, props.zoneName]);

  useEffect(() => {
    if (!proxied) return;
    if (recordType === "A" || recordType === "AAAA") {
      setOriginIp((current) => (current ? current : recordValue));
    }
  }, [proxied, recordType, recordValue]);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setLocalError("");
    if (proxied && proxyTlsEnabled && !proxyCertificateId) {
      setLocalError("Select a certificate for HTTPS proxying");
      return;
    }
    if (proxied && basicAuthEnabled) {
      if (!proxyTlsEnabled) {
        setLocalError("Basic Auth requires client HTTPS");
        return;
      }
      if (!basicAuthUsername.trim()) {
        setLocalError("Basic Auth username is required");
        return;
      }
      if (!editing && !basicAuthPassword) {
        setLocalError("Basic Auth password is required");
        return;
      }
      if (editing && !basicAuthPassword && !existingSecretId) {
        setLocalError("Basic Auth password is required");
        return;
      }
    }

    const proxy = proxied
      ? {
          origin: {
            ip:
              originIp.trim() ||
              (recordType === "A" || recordType === "AAAA" ? recordValue : ""),
            port: Number(originPort),
            protocol: originProtocol,
          },
          tlsEnabled: proxyTlsEnabled,
          certificateId: proxyTlsEnabled ? proxyCertificateId : undefined,
          redirectHttpToHttps: proxyTlsEnabled ? redirectHttpToHttps : false,
          http2: proxyTlsEnabled ? http2 : false,
          http3: proxyTlsEnabled ? http3 : false,
          websocket,
          cache: { enabled: cacheEnabled },
          backendTlsVerify:
            originProtocol === "https" ? backendTlsVerify : false,
          timeouts: {
            connectSeconds,
            sendReadSeconds,
            clientHeaderSeconds,
            bodyLimitMb,
          },
          nginxDirectives: nginxDirectives.trim() || undefined,
          pathRules: pathRules
            .filter((rule) => rule.pattern.trim())
            .map((rule) =>
              rule.actionType === "redirect"
                ? {
                    kind: rule.kind,
                    pattern: rule.pattern,
                    action: {
                      type: "redirect" as const,
                      status: rule.status,
                      location: rule.location || "/",
                    },
                  }
                : {
                    kind: rule.kind,
                    pattern: rule.pattern,
                    action: {
                      type: "proxy" as const,
                      rewrite: rule.rewrite.trim() || undefined,
                    },
                  },
            ),
          basicAuth: basicAuthEnabled
            ? {
                username: basicAuthUsername.trim(),
                ...(basicAuthPassword
                  ? { password: basicAuthPassword }
                  : existingSecretId
                    ? { passwordSecretId: existingSecretId }
                    : {}),
              }
            : undefined,
        }
      : undefined;

    const saved = await props.onSubmit({
      name: recordName,
      type: recordType,
      value: parseRecordValue(recordType, recordValue),
      enabled: true,
      proxied,
      proxy,
    });
    if (!saved) return;
  }

  if (!props.open) return null;

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center bg-bay/85 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="record-dialog-title"
    >
      <form
        className="pc-panel max-h-[90vh] w-full max-w-3xl overflow-y-auto p-6 shadow-2xl shadow-black/40"
        onSubmit={handleSubmit}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">DNS record</p>
            <h2
              id="record-dialog-title"
              className="pc-title mt-2 text-2xl text-mist"
            >
              {editing ? "Configure record" : "Configure new record"}
            </h2>
            <p className="mt-2 text-sm text-mute">
              Choose whether ProxyCore serves this record directly or proxies it
              through Nginx.
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

        {proxied ? (
          <div
            className="mt-6 overflow-x-auto border-b border-line"
            role="tablist"
            aria-label="Record configuration"
          >
            <div className="flex min-w-max gap-1">
              {proxyTabs.map((tab) => (
                <button
                  key={tab.id}
                  type="button"
                  role="tab"
                  aria-selected={activeTab === tab.id}
                  onClick={() => setActiveTab(tab.id)}
                  className={`rounded-t-xl border-b-2 px-4 py-3 text-left transition ${
                    activeTab === tab.id
                      ? "border-signal bg-signal/10 text-signal"
                      : "border-transparent text-faint hover:bg-raised/80 hover:text-mist"
                  }`}
                >
                  <span className="block text-sm font-medium">{tab.label}</span>
                  <span className="mt-1 block text-[11px] text-faint">
                    {tab.hint}
                  </span>
                </button>
              ))}
            </div>
          </div>
        ) : null}

        {!proxied || activeTab === "record" ? (
          <div className="mt-6 grid gap-4 md:grid-cols-2">
            <label className={labelClass}>
              Name
              <input
                value={recordName}
                onChange={(event) => setRecordName(event.target.value)}
                className={inputClass}
                placeholder="app"
                aria-label="Record name"
              />
            </label>
            <label className={labelClass}>
              Type
              <select
                value={recordType}
                onChange={(event) => setRecordType(event.target.value)}
                className={inputClass}
                aria-label="Record type"
              >
                {["A", "AAAA", "CNAME", "TXT", "MX", "SRV"].map((type) => (
                  <option key={type}>{type}</option>
                ))}
              </select>
            </label>
            {!proxied || recordType === "CNAME" ? (
              <label className={`${labelClass} md:col-span-2`}>
                {proxied ? "DNS target" : "Value"}
                <input
                  value={recordValue}
                  onChange={(event) => setRecordValue(event.target.value)}
                  className={inputClass}
                  placeholder={
                    recordType === "CNAME" ? "target.home.arpa" : "192.168.1.20"
                  }
                  aria-label={proxied ? "DNS target" : "Record value"}
                />
              </label>
            ) : (
              <div className="rounded-xl border border-signal/25 bg-signal/10 p-4 text-sm leading-6 text-mist/90 md:col-span-2">
                The DNS answer comes from the configured proxy ingress. Set the
                upstream target in the Origin tab.
              </div>
            )}
            <label className="flex items-center gap-3 text-sm text-mist/90 md:col-span-2">
              <input
                type="checkbox"
                checked={proxied}
                onChange={(event) => {
                  const enabled = event.target.checked;
                  setProxied(enabled);
                  setActiveTab(enabled ? "origin" : "record");
                }}
                className="size-4 accent-signal"
                disabled={!["A", "AAAA", "CNAME"].includes(recordType)}
              />
              Proxy through ProxyCore
            </label>
          </div>
        ) : null}

        {proxied ? (
          <div className="mt-6 space-y-4">
            {activeTab === "origin" ? (
              <section className={sectionClass}>
                <p className="pc-eyebrow">
                  Origin
                </p>
                <div className="mt-4 grid gap-4 md:grid-cols-3">
                  <label className={`${labelClass} md:col-span-1`}>
                    Upstream IP
                    <input
                      value={originIp}
                      onChange={(event) => setOriginIp(event.target.value)}
                      className={inputClass}
                      required
                    />
                  </label>
                  <label className={labelClass}>
                    Port
                    <input
                      type="number"
                      min={1}
                      max={65535}
                      value={originPort}
                      onChange={(event) =>
                        setOriginPort(Number(event.target.value))
                      }
                      className={inputClass}
                      required
                    />
                  </label>
                  <label className={labelClass}>
                    Upstream protocol
                    <select
                      value={originProtocol}
                      onChange={(event) =>
                        setOriginProtocol(
                          event.target.value === "https" ? "https" : "http",
                        )
                      }
                      className={inputClass}
                    >
                      <option value="http">HTTP</option>
                      <option value="https">HTTPS</option>
                    </select>
                  </label>
                </div>
                {recordType === "CNAME" ? (
                  <p className="mt-3 text-xs leading-5 text-faint">
                    Proxied CNAME records require an explicit literal origin IP.
                  </p>
                ) : null}
              </section>
            ) : null}

            {activeTab === "tls" ? (
              <section className={sectionClass}>
                <p className="pc-eyebrow">
                  Client TLS
                </p>
                <div className="mt-4 grid gap-4 md:grid-cols-2">
                  <label className={labelClass}>
                    Client protocol
                    <select
                      value={proxyTlsEnabled ? "https" : "http"}
                      onChange={(event) => {
                        const https = event.target.value === "https";
                        setProxyTlsEnabled(https);
                        setHttp2(https);
                        setRedirectHttpToHttps(https);
                        if (!https) {
                          setHttp3(false);
                          setBasicAuthEnabled(false);
                        }
                      }}
                      className={inputClass}
                    >
                      <option value="https">HTTPS</option>
                      <option value="http">HTTP</option>
                    </select>
                  </label>
                  {proxyTlsEnabled ? (
                    <label className={labelClass}>
                      Certificate
                      <select
                        value={proxyCertificateId}
                        onChange={(event) =>
                          setProxyCertificateId(event.target.value)
                        }
                        className={inputClass}
                        required
                      >
                        <option value="">
                          Select a configured certificate
                        </option>
                        {availableCertificates.map((certificate) => (
                          <option key={certificate.id} value={certificate.id}>
                            {certificate.hostnames.join(", ")} ·{" "}
                            {certificate.issuer}
                          </option>
                        ))}
                      </select>
                    </label>
                  ) : null}
                  {proxyTlsEnabled ? (
                    <label className="flex items-center gap-3 text-sm text-mist/90 md:col-span-2">
                      <input
                        type="checkbox"
                        checked={redirectHttpToHttps}
                        onChange={(event) =>
                          setRedirectHttpToHttps(event.target.checked)
                        }
                        className="size-4 accent-signal"
                      />
                      Redirect HTTP to HTTPS
                    </label>
                  ) : null}
                </div>
                {proxyTlsEnabled && availableCertificates.length === 0 ? (
                  <p className="mt-4 rounded-xl border border-amber-300/30 bg-amber-300/10 p-3 text-sm text-amber-100">
                    HTTPS requires an active configured certificate for this
                    hostname before the record can be applied.
                  </p>
                ) : null}
              </section>
            ) : null}

            {activeTab === "nginx" ? (
              <section className={sectionClass}>
                <p className="pc-eyebrow">
                  Nginx
                </p>
                <div className="mt-4 grid gap-3 md:grid-cols-2">
                  <Toggle
                    label="HTTP/2"
                    checked={http2}
                    disabled={!proxyTlsEnabled}
                    onChange={setHttp2}
                  />
                  <Toggle
                    label="HTTP/3"
                    checked={http3}
                    disabled={!proxyTlsEnabled}
                    onChange={setHttp3}
                  />
                  <Toggle
                    label="WebSocket"
                    checked={websocket}
                    onChange={setWebsocket}
                  />
                  <Toggle
                    label="Cache GET/HEAD"
                    checked={cacheEnabled}
                    disabled={basicAuthEnabled || websocket}
                    onChange={setCacheEnabled}
                  />
                  {originProtocol === "https" ? (
                    <Toggle
                      label="Verify upstream TLS"
                      checked={backendTlsVerify}
                      onChange={setBackendTlsVerify}
                    />
                  ) : null}
                </div>
                <div className="mt-4 grid gap-4 md:grid-cols-4">
                  <NumberField
                    label="Connect (s)"
                    value={connectSeconds}
                    onChange={setConnectSeconds}
                  />
                  <NumberField
                    label="Send/read (s)"
                    value={sendReadSeconds}
                    onChange={setSendReadSeconds}
                  />
                  <NumberField
                    label="Client header (s)"
                    value={clientHeaderSeconds}
                    onChange={setClientHeaderSeconds}
                  />
                  <NumberField
                    label="Body limit (MB)"
                    value={bodyLimitMb}
                    onChange={setBodyLimitMb}
                  />
                </div>
                <div className="mt-5">
                  <label className={labelClass}>
                    Custom Nginx directives
                    <textarea
                      value={nginxDirectives}
                      onChange={(event) =>
                        setNginxDirectives(event.target.value)
                      }
                      className={`${inputClass} min-h-56 resize-y font-mono text-xs leading-6`}
                      placeholder={`client_max_body_size 100m;
add_header X-Robots-Tag "noindex" always;
proxy_set_header X-Environment "homelab";`}
                      spellCheck={false}
                      rows={10}
                    />
                  </label>
                  <p className="mt-2 text-xs leading-5 text-faint">
                    Add server-level Nginx directives, one per line. Generated
                    defaults with the same directive name are omitted so you can
                    override values such as client body size or proxy timeouts.
                    Blocks with braces are not supported here.
                  </p>
                </div>
              </section>
            ) : null}

            {activeTab === "routes" ? (
              <section className={sectionClass}>
                <div className="flex items-center justify-between">
                  <p className="pc-eyebrow">
                    Redirects / paths
                  </p>
                  <button
                    type="button"
                    className="text-xs text-link hover:underline"
                    onClick={() =>
                      setPathRules((current) => [
                        ...current,
                        {
                          kind: "exact",
                          pattern: "/health",
                          actionType: "redirect",
                          status: 307,
                          location: "/",
                          rewrite: "",
                        },
                      ])
                    }
                  >
                    Add path rule
                  </button>
                </div>
                <div className="mt-4 space-y-3">
                  {pathRules.length === 0 ? (
                    <p className="text-sm text-faint">
                      No path rules yet. Requests fall through to the origin at
                      `/`.
                    </p>
                  ) : (
                    pathRules.map((rule, index) => (
                      <div
                        key={index}
                        className="grid gap-2 rounded-xl border border-line p-3 md:grid-cols-2"
                      >
                        <label className={labelClass}>
                          Kind
                          <select
                            value={rule.kind}
                            onChange={(event) =>
                              setPathRules((current) =>
                                current.map((item, itemIndex) =>
                                  itemIndex === index
                                    ? {
                                        ...item,
                                        kind:
                                          event.target.value === "exact"
                                            ? "exact"
                                            : "prefix",
                                      }
                                    : item,
                                ),
                              )
                            }
                            className={inputClass}
                          >
                            <option value="exact">Exact</option>
                            <option value="prefix">Prefix</option>
                          </select>
                        </label>
                        <label className={labelClass}>
                          Pattern
                          <input
                            value={rule.pattern}
                            onChange={(event) =>
                              setPathRules((current) =>
                                current.map((item, itemIndex) =>
                                  itemIndex === index
                                    ? { ...item, pattern: event.target.value }
                                    : item,
                                ),
                              )
                            }
                            className={inputClass}
                            placeholder="/api"
                          />
                        </label>
                        <label className={labelClass}>
                          Action
                          <select
                            value={rule.actionType}
                            onChange={(event) =>
                              setPathRules((current) =>
                                current.map((item, itemIndex) =>
                                  itemIndex === index
                                    ? {
                                        ...item,
                                        actionType:
                                          event.target.value === "redirect"
                                            ? "redirect"
                                            : "proxy",
                                      }
                                    : item,
                                ),
                              )
                            }
                            className={inputClass}
                          >
                            <option value="redirect">Redirect</option>
                            <option value="proxy">Proxy</option>
                          </select>
                        </label>
                        {rule.actionType === "redirect" ? (
                          <>
                            <label className={labelClass}>
                              Status
                              <select
                                value={rule.status}
                                onChange={(event) =>
                                  setPathRules((current) =>
                                    current.map((item, itemIndex) =>
                                      itemIndex === index
                                        ? {
                                            ...item,
                                            status: Number(
                                              event.target.value,
                                            ) as 301 | 302 | 307 | 308,
                                          }
                                        : item,
                                    ),
                                  )
                                }
                                className={inputClass}
                              >
                                {[301, 302, 307, 308].map((status) => (
                                  <option key={status} value={status}>
                                    {status}
                                  </option>
                                ))}
                              </select>
                            </label>
                            <label className={`${labelClass} md:col-span-2`}>
                              Location
                              <input
                                value={rule.location}
                                onChange={(event) =>
                                  setPathRules((current) =>
                                    current.map((item, itemIndex) =>
                                      itemIndex === index
                                        ? {
                                            ...item,
                                            location: event.target.value,
                                          }
                                        : item,
                                    ),
                                  )
                                }
                                className={inputClass}
                                placeholder="/"
                              />
                            </label>
                          </>
                        ) : (
                          <label className={`${labelClass} md:col-span-1`}>
                            Rewrite
                            <input
                              value={rule.rewrite}
                              onChange={(event) =>
                                setPathRules((current) =>
                                  current.map((item, itemIndex) =>
                                    itemIndex === index
                                      ? { ...item, rewrite: event.target.value }
                                      : item,
                                  ),
                                )
                              }
                              className={inputClass}
                              placeholder="/v1"
                            />
                          </label>
                        )}
                        <button
                          type="button"
                          className="pc-btn-ghost justify-self-start !text-sm"
                          onClick={() =>
                            setPathRules((current) =>
                              current.filter(
                                (_, itemIndex) => itemIndex !== index,
                              ),
                            )
                          }
                        >
                          Remove rule
                        </button>
                      </div>
                    ))
                  )}
                </div>
              </section>
            ) : null}

            {activeTab === "access" ? (
              <section className={sectionClass}>
                <p className="pc-eyebrow">
                  Basic Auth
                </p>
                <label className="mt-4 flex items-center gap-3 text-sm text-mist/90">
                  <input
                    type="checkbox"
                    checked={basicAuthEnabled}
                    onChange={(event) => {
                      const enabled = event.target.checked;
                      setBasicAuthEnabled(enabled);
                      if (enabled) {
                        setCacheEnabled(false);
                        setProxyTlsEnabled(true);
                        setActiveTab("access");
                      }
                    }}
                    className="size-4 accent-signal"
                  />
                  Require HTTP Basic Auth
                </label>
                {basicAuthEnabled ? (
                  <div className="mt-4 grid gap-4 md:grid-cols-2">
                    <label className={labelClass}>
                      Username
                      <input
                        value={basicAuthUsername}
                        onChange={(event) =>
                          setBasicAuthUsername(event.target.value)
                        }
                        className={inputClass}
                        required
                      />
                    </label>
                    <label className={labelClass}>
                      Password
                      <input
                        type="password"
                        value={basicAuthPassword}
                        onChange={(event) =>
                          setBasicAuthPassword(event.target.value)
                        }
                        className={inputClass}
                        placeholder={
                          existingSecretId
                            ? "Leave blank to keep current password"
                            : "Minimum 8 characters"
                        }
                        required={!existingSecretId}
                      />
                    </label>
                  </div>
                ) : null}
              </section>
            ) : null}
          </div>
        ) : null}

        {localError ? (
          <p className="pc-toast-err !mt-4" role="alert">
            {localError}
          </p>
        ) : null}

        <div className="mt-6 flex justify-end gap-3">
          <button
            type="button"
            className="pc-btn-ghost"
            onClick={props.onClose}
          >
            Cancel
          </button>
          <button
            className="pc-btn"
            type="submit"
          >
            {editing ? "Save changes" : "Save record"}
          </button>
        </div>
      </form>
    </div>
  );
}

function Toggle(props: {
  label: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (value: boolean) => void;
}) {
  return (
    <label
      className={`flex items-center gap-3 text-sm ${
        props.disabled ? "text-faint" : "text-mist/90"
      }`}
    >
      <input
        type="checkbox"
        checked={props.checked}
        disabled={props.disabled}
        onChange={(event) => props.onChange(event.target.checked)}
        className="size-4 accent-signal"
      />
      {props.label}
    </label>
  );
}

function NumberField(props: {
  label: string;
  value: number;
  onChange: (value: number) => void;
}) {
  return (
    <label className={labelClass}>
      {props.label}
      <input
        type="number"
        min={1}
        value={props.value}
        onChange={(event) => props.onChange(Number(event.target.value))}
        className={inputClass}
      />
    </label>
  );
}

function shortName(fullName: string, zoneName?: string): string {
  if (!zoneName) return fullName;
  if (fullName === zoneName) return "@";
  if (fullName.endsWith(`.${zoneName}`)) {
    return fullName.slice(0, -(zoneName.length + 1));
  }
  return fullName;
}

function parseRecordValue(type: string, value: string): unknown {
  if (type === "MX") {
    try {
      return JSON.parse(value) as unknown;
    } catch {
      return value;
    }
  }
  if (type === "SRV") {
    try {
      return JSON.parse(value) as unknown;
    } catch {
      return value;
    }
  }
  return value;
}

function formatHeaderDirectives(
  headers: Array<{ name: string; value: string }> | undefined,
): string {
  return (headers ?? [])
    .map(
      (header) =>
        `proxy_set_header ${header.name} ${formatNginxValue(header.value)};`,
    )
    .join("\n");
}

function formatNginxValue(value: string): string {
  return /^[a-zA-Z0-9_./:$-]+$/.test(value)
    ? value
    : `"${value.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`;
}

function canonicalRecordName(name: string, zoneName?: string): string {
  const value = name.trim().replace(/\.+$/, "").toLowerCase();
  if (!zoneName || value === "@" || value === "") {
    return (value === "@" || value === "" ? zoneName : value) ?? value;
  }
  const zone = zoneName.trim().replace(/\.+$/, "").toLowerCase();
  return value === zone || value.endsWith(`.${zone}`)
    ? value
    : `${value}.${zone}`;
}

function certificateCoversHostname(
  certificateHostnames: string[],
  hostname: string,
): boolean {
  const normalizedHostname = hostname.toLowerCase().replace(/\.+$/, "");
  return certificateHostnames.some((candidate) => {
    const normalizedCandidate = candidate.toLowerCase().replace(/\.+$/, "");
    return (
      normalizedCandidate === normalizedHostname ||
      (normalizedCandidate.startsWith("*.") &&
        normalizedHostname.endsWith(normalizedCandidate.slice(1)) &&
        normalizedHostname.split(".").length ===
          normalizedCandidate.split(".").length)
    );
  });
}
