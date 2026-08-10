import { FormEvent, useMemo, useState } from "react";

type CertificateMode = "self-signed" | "uploaded" | "letsencrypt";
type Challenge = "http-01" | "dns-01";

export type DashboardCertificate = {
  id: string;
  hostnames: string[];
  issuer: string;
  challenge: string;
  environment: string;
  status: string;
  expiresAt?: string;
  renewAfter?: string;
  failureReason?: string;
};

export function CertificatesView(props: {
  certificates: DashboardCertificate[];
  onRefresh: () => Promise<void>;
  onMessage: (message: string) => void;
  onError: (message: string) => void;
}) {
  const [mode, setMode] = useState<CertificateMode>("self-signed");
  const [hostnames, setHostnames] = useState("app.home.arpa");
  const [environment, setEnvironment] = useState("staging");
  const [challenge, setChallenge] = useState<Challenge>("http-01");
  const [keyType, setKeyType] = useState<"rsa" | "ecdsa">("rsa");
  const [propagationSeconds, setPropagationSeconds] = useState(30);
  const [email, setEmail] = useState("");
  const [certificateFile, setCertificateFile] = useState<File>();
  const [privateKeyFile, setPrivateKeyFile] = useState<File>();
  const [cloudflareApiToken, setCloudflareApiToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [downloadingCA, setDownloadingCA] = useState(false);

  const names = useMemo(
    () =>
      hostnames
        .split(/[,\n]/)
        .map((hostname) => hostname.trim())
        .filter(Boolean),
    [hostnames],
  );
  const hasWildcard = names.some((hostname) => hostname.startsWith("*."));

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    props.onMessage("");
    props.onError("");
    if (names.length === 0) {
      props.onError("Enter at least one domain or wildcard");
      return;
    }
    if (mode === "uploaded" && (!certificateFile || !privateKeyFile)) {
      props.onError("Choose both the certificate and private key files");
      return;
    }
    if (mode === "letsencrypt" && challenge === "http-01" && hasWildcard) {
      props.onError(
        "Let's Encrypt wildcard certificates require the DNS-01 challenge",
      );
      return;
    }
    if (
      mode === "letsencrypt" &&
      challenge === "dns-01" &&
      !cloudflareApiToken.trim()
    ) {
      props.onError("Enter a Cloudflare API token for DNS-01");
      return;
    }
    setSubmitting(true);
    try {
      const isUpload = mode === "uploaded";
      const body = isUpload
        ? createUploadBody()
        : JSON.stringify({
            hostnames: names,
            issuer: mode,
            challenge: mode === "letsencrypt" ? challenge : "none",
            keyType: mode === "letsencrypt" ? keyType : undefined,
            propagationSeconds:
              mode === "letsencrypt" && challenge === "dns-01"
                ? propagationSeconds
                : undefined,
            environment: mode === "letsencrypt" ? environment : "local",
            email: email.trim() || undefined,
            cloudflare:
              mode === "letsencrypt" && challenge === "dns-01"
                ? {
                    apiToken: cloudflareApiToken.trim() || undefined,
                  }
                : undefined,
          });
      const response = await fetch("/api/certificates", {
        method: "POST",
        credentials: "include",
        ...(isUpload
          ? { body }
          : {
              headers: { "content-type": "application/json" },
              body,
            }),
      });
      const payload = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) {
        props.onError(payload.error ?? "Certificate request failed");
        await props.onRefresh();
        return;
      }
      props.onMessage(
        mode === "self-signed"
          ? "Self-signed certificate created"
          : mode === "uploaded"
            ? "Certificate uploaded and validated"
            : "Let's Encrypt certificate issued",
      );
      setCertificateFile(undefined);
      setPrivateKeyFile(undefined);
      await props.onRefresh();
    } catch {
      props.onError("Certificate request could not be completed");
    } finally {
      setSubmitting(false);
    }
  }

  function createUploadBody(): FormData {
    const body = new FormData();
    body.set("hostnames", names.join(","));
    body.set("issuer", "uploaded");
    body.set("challenge", "none");
    body.set("environment", "local");
    body.set("certificate", certificateFile!);
    body.set("privateKey", privateKeyFile!);
    return body;
  }

  async function downloadTrustCA() {
    props.onMessage("");
    props.onError("");
    setDownloadingCA(true);
    try {
      const response = await fetch("/api/certificates/ca", {
        credentials: "include",
      });
      if (!response.ok) {
        const payload = (await response.json().catch(() => ({}))) as {
          error?: string;
        };
        props.onError(payload.error ?? "Could not download trust certificate");
        return;
      }
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = "proxycore-ca.crt";
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
      props.onMessage(
        "Downloaded proxycore-ca.crt — install it once in your OS trust store",
      );
    } catch {
      props.onError("Could not download trust certificate");
    } finally {
      setDownloadingCA(false);
    }
  }

  return (
    <div className="mt-8 space-y-6">
      <section className="pc-panel overflow-hidden">
        <div className="grid gap-8 p-6 lg:grid-cols-[0.8fr_1.2fr] lg:p-8">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">Certificate desk</p>
            <h2 className="pc-title mt-4 max-w-md text-3xl text-mist">
              Put a trusted name in front of your service.
            </h2>
            <p className="mt-4 max-w-md text-sm leading-6 text-mute">
              One certificate can cover several names, including a one-label
              wildcard such as{" "}
              <code className="font-mono text-link">*.home.arpa</code>. Private
              keys stay encrypted and never return to this screen.
            </p>
            <div className="mt-8 grid grid-cols-3 gap-2 text-center text-xs">
              <DeskMetric
                value={String(props.certificates.length)}
                label="stored"
              />
              <DeskMetric
                value={String(
                  props.certificates.filter((certificate) =>
                    ["active", "issued"].includes(certificate.status),
                  ).length,
                )}
                label="usable"
              />
              <DeskMetric
                value={String(
                  props.certificates.filter(
                    (certificate) => certificate.status === "failed",
                  ).length,
                )}
                label="attention"
              />
            </div>
          </div>

          <form onSubmit={submit} className="pc-panel-quiet p-5">
            <div
              className="flex flex-wrap gap-2"
              role="tablist"
              aria-label="Certificate source"
            >
              <ModeButton
                active={mode === "self-signed"}
                onClick={() => setMode("self-signed")}
              >
                Auto-generate
              </ModeButton>
              <ModeButton
                active={mode === "uploaded"}
                onClick={() => setMode("uploaded")}
              >
                Upload PEM
              </ModeButton>
              <ModeButton
                active={mode === "letsencrypt"}
                onClick={() => setMode("letsencrypt")}
              >
                Let&apos;s Encrypt
              </ModeButton>
            </div>

            <label className="pc-label mt-6">
              Domain names
              <textarea
                value={hostnames}
                onChange={(event) => setHostnames(event.target.value)}
                className={inputClass}
                rows={2}
                placeholder={"app.example.com\n*.example.com"}
                aria-label="Certificate domain names"
              />
              <span className="mt-2 block text-xs text-faint">
                Separate names with commas or line breaks.
              </span>
            </label>

            {mode === "self-signed" ? (
              <div className="mt-5 space-y-3 rounded-xl border border-signal/25 bg-signal/10 p-4 text-sm leading-6 text-mist/90">
                <p>
                  Issues a 1-year leaf certificate signed by this installation&apos;s
                  private CA. Download the CA once and trust it on your PC —
                  renewals keep working without reinstalling.
                </p>
                <button
                  type="button"
                  className="pc-btn-ghost !text-xs"
                  disabled={downloadingCA}
                  onClick={() => void downloadTrustCA()}
                >
                  {downloadingCA
                    ? "Preparing download…"
                    : "Download trust certificate (.crt)"}
                </button>
              </div>
            ) : null}

            {mode === "uploaded" ? (
              <div className="mt-5 grid gap-4 md:grid-cols-2">
                <FileField
                  label="Certificate chain (.pem / .crt)"
                  onChange={setCertificateFile}
                />
                <FileField
                  label="Private key (.pem / .key)"
                  onChange={setPrivateKeyFile}
                />
                <p className="text-xs leading-5 text-faint md:col-span-2">
                  The certificate is checked for expiry, SAN coverage, and a
                  matching private key before it is installed.
                </p>
              </div>
            ) : null}

            {mode === "letsencrypt" ? (
              <div className="mt-5 space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <label className={labelClass}>
                    Challenge
                    <select
                      value={challenge}
                      onChange={(event) =>
                        setChallenge(
                          event.target.value === "dns-01"
                            ? "dns-01"
                            : "http-01",
                        )
                      }
                      className={inputClass}
                    >
                      <option value="http-01">HTTP-01 · public port 80</option>
                      <option value="dns-01">DNS-01 · Cloudflare</option>
                    </select>
                  </label>
                  <label className={labelClass}>
                    Key type
                    <select
                      value={keyType}
                      onChange={(event) =>
                        setKeyType(
                          event.target.value === "ecdsa" ? "ecdsa" : "rsa",
                        )
                      }
                      className={inputClass}
                    >
                      <option value="rsa">RSA · broadly compatible</option>
                      <option value="ecdsa">
                        ECDSA P-256 · modern clients
                      </option>
                    </select>
                  </label>
                </div>
                {challenge === "http-01" ? (
                  <p className="rounded-xl border border-link/25 bg-link/10 p-4 text-xs leading-5 text-link">
                    Point the domain to this installation and make port 80
                    reachable from the Internet while Let&apos;s Encrypt checks
                    the challenge.
                  </p>
                ) : (
                  <div className="space-y-4">
                    <div className="grid gap-4 md:grid-cols-[0.45fr_1fr]">
                      <label className={labelClass}>
                        Propagation seconds
                        <input
                          type="number"
                          min={0}
                          max={600}
                          value={propagationSeconds}
                          onChange={(event) =>
                            setPropagationSeconds(Number(event.target.value))
                          }
                          className={inputClass}
                        />
                      </label>
                      <label className={labelClass}>
                        Cloudflare API token
                        <input
                          type="password"
                          value={cloudflareApiToken}
                          onChange={(event) =>
                            setCloudflareApiToken(event.target.value)
                          }
                          className={inputClass}
                          placeholder="DNS Write + Zone Read"
                        />
                      </label>
                    </div>
                    <p className="text-xs leading-5 text-faint">
                      ProxyCore discovers the Cloudflare zone from the requested
                      domain. The token is encrypted before persistence and is
                      only used for <code>_acme-challenge</code> TXT records.
                    </p>
                  </div>
                )}
                {hasWildcard && challenge === "http-01" ? (
                  <p className="rounded-xl border border-signal/30 bg-signal/10 p-3 text-xs text-signal">
                    Wildcards need DNS-01. Switch the challenge above.
                  </p>
                ) : null}
                <details className="pc-panel-quiet p-4">
                  <summary className="pc-eyebrow cursor-pointer">
                    Advanced issuance options
                  </summary>
                  <div className="mt-4 grid gap-4 md:grid-cols-2">
                    <label className={labelClass}>
                      Directory
                      <select
                        value={environment}
                        onChange={(event) => setEnvironment(event.target.value)}
                        className={inputClass}
                      >
                        <option value="staging">
                          Staging · safe for tests
                        </option>
                        <option value="production">Production</option>
                      </select>
                    </label>
                    <label className={labelClass}>
                      Account email{" "}
                      <span className="text-faint">(optional)</span>
                      <input
                        type="email"
                        value={email}
                        onChange={(event) => setEmail(event.target.value)}
                        className={inputClass}
                        placeholder="ops@example.com"
                      />
                    </label>
                  </div>
                </details>
              </div>
            ) : null}

            <button type="submit" disabled={submitting} className="pc-btn mt-6">
              {submitting
                ? mode === "letsencrypt"
                  ? "Waiting for certificate authority…"
                  : "Validating…"
                : mode === "self-signed"
                  ? "Generate self-signed"
                  : mode === "uploaded"
                    ? "Validate and install"
                    : "Request from Let's Encrypt"}
            </button>
          </form>
        </div>
      </section>

      <section className="pc-panel p-6">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <p className="pc-eyebrow">Certificate inventory</p>
            <h2 className="pc-title mt-2 text-2xl text-mist">
              What Nginx can use
            </h2>
          </div>
          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              className="pc-btn-ghost !text-xs"
              disabled={downloadingCA}
              onClick={() => void downloadTrustCA()}
            >
              {downloadingCA ? "Preparing…" : "Download trust CA"}
            </button>
            <button
              type="button"
              className="pc-btn-ghost !text-xs"
              onClick={() => void props.onRefresh()}
            >
              Refresh inventory
            </button>
          </div>
        </div>
        <div className="mt-6 grid gap-3">
          {props.certificates.length ? (
            props.certificates.map((certificate) => (
              <CertificateCard key={certificate.id} certificate={certificate} />
            ))
          ) : (
            <p className="rounded-xl border border-dashed border-line p-5 text-sm text-faint">
              No certificates yet. Generate an internal certificate or request
              one from a public certificate authority.
            </p>
          )}
        </div>
      </section>
    </div>
  );
}

function DeskMetric(props: { value: string; label: string }) {
  return (
    <div className="pc-panel-quiet px-2 py-3">
      <p className="font-mono text-lg text-link">{props.value}</p>
      <p className="mt-1 uppercase tracking-[0.16em] text-faint">{props.label}</p>
    </div>
  );
}

function ModeButton(props: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={props.active}
      onClick={props.onClick}
      className={`rounded-lg px-3 py-2 text-xs transition ${
        props.active
          ? "bg-signal font-semibold text-[#1a120c]"
          : "bg-bay text-mute hover:text-mist"
      }`}
    >
      {props.children}
    </button>
  );
}

function FileField(props: {
  label: string;
  onChange: (file: File | undefined) => void;
}) {
  return (
    <label className={labelClass}>
      {props.label}
      <input
        type="file"
        accept=".pem,.crt,.key,application/x-pem-file"
        onChange={(event) => props.onChange(event.target.files?.[0])}
        className="mt-2 block w-full rounded-xl border border-dashed border-line bg-bay px-3 py-3 text-xs text-mute file:mr-3 file:rounded-lg file:border-0 file:bg-raised file:px-2 file:py-1.5 file:text-xs file:text-mist"
      />
    </label>
  );
}

function CertificateCard(props: { certificate: DashboardCertificate }) {
  const { certificate } = props;
  const expires = certificate.expiresAt
    ? new Date(certificate.expiresAt)
    : undefined;
  const daysRemaining = expires
    ? Math.ceil((expires.getTime() - Date.now()) / (24 * 60 * 60 * 1_000))
    : undefined;
  const statusTone =
    certificate.status === "active" || certificate.status === "issued"
      ? "text-ok"
      : certificate.status === "failed"
        ? "text-danger"
        : "text-signal";
  return (
    <article className="grid gap-4 rounded-xl border border-line/80 bg-bay/50 p-4 md:grid-cols-[1fr_auto] md:items-center">
      <div className="min-w-0">
        <p className="truncate text-sm text-mist">
          {certificate.hostnames.join(", ")}
        </p>
        <p className="mt-2 font-mono text-[11px] uppercase tracking-[0.08em] text-faint">
          {certificate.issuer} · {certificate.challenge} ·{" "}
          {certificate.environment}
        </p>
        {certificate.failureReason ? (
          <p className="mt-2 text-xs leading-5 text-danger">
            {certificate.failureReason}
          </p>
        ) : null}
      </div>
      <div className="text-left md:text-right">
        <p className={`text-sm font-medium ${statusTone}`}>
          {certificate.status}
        </p>
        <p className="mt-1 text-xs text-faint">
          {expires
            ? daysRemaining !== undefined && daysRemaining >= 0
              ? `${daysRemaining} days left · ${expires.toLocaleDateString()}`
              : `Expired · ${expires.toLocaleDateString()}`
            : "Expiry pending"}
        </p>
      </div>
    </article>
  );
}

const inputClass = "pc-input";
const labelClass = "pc-label";
