import { FormEvent, useMemo, useState } from "react";
import { createPortal } from "react-dom";

type CertificateMode = "self-signed" | "uploaded" | "letsencrypt";
type Challenge = "http-01" | "dns-01";

const inputClass = "pc-input";
const labelClass = "pc-label";

export function CertificateRequestDialog(props: {
  open: boolean;
  onClose: () => void;
  onSuccess: (message: string) => void;
  onError: (message: string) => void;
  onRefresh: () => Promise<void>;
}) {
  const [mode, setMode] = useState<CertificateMode>("self-signed");
  const [hostnames, setHostnames] = useState("app.home.arpa");
  const [environment, setEnvironment] = useState("staging");
  const [challenge, setChallenge] = useState<Challenge>("http-01");
  const [keyType, setKeyType] = useState<"rsa" | "ecdsa">("rsa");
  const [propagationSeconds, setPropagationSeconds] = useState(60);
  const [email, setEmail] = useState("");
  const [certificateFile, setCertificateFile] = useState<File>();
  const [privateKeyFile, setPrivateKeyFile] = useState<File>();
  const [cloudflareApiToken, setCloudflareApiToken] = useState("");
  const [submitting, setSubmitting] = useState(false);

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
      props.onSuccess(
        mode === "self-signed"
          ? "Self-signed certificate created"
          : mode === "uploaded"
            ? "Certificate uploaded and validated"
            : "Let's Encrypt certificate issued",
      );
      setCertificateFile(undefined);
      setPrivateKeyFile(undefined);
      setHostnames("app.home.arpa");
      await props.onRefresh();
      props.onClose();
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

  if (!props.open) return null;

  const submitLabel =
    submitting
      ? mode === "letsencrypt"
        ? "Waiting for certificate authority…"
        : "Validating…"
      : mode === "self-signed"
        ? "Generate self-signed"
        : mode === "uploaded"
          ? "Validate and install"
          : "Request from Let's Encrypt";

  return createPortal(
    <div
      className="fixed inset-0 z-50 grid place-items-center bg-bay/85 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="cert-dialog-title"
    >
      <form
        className="pc-panel max-h-[90vh] w-full max-w-4xl overflow-y-auto p-6 font-mono"
        onSubmit={submit}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="pc-eyebrow pc-eyebrow-signal">certificate desk</p>
            <h2
              id="cert-dialog-title"
              className="pc-title mt-2 text-2xl text-mist"
            >
              request a certificate
            </h2>
            <p className="mt-2 text-sm text-mute">
              Self-signed for internal services, Let&apos;s Encrypt for public
              ones. Keys never return to the browser.
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

        <div
          className="mt-6 flex flex-wrap gap-2"
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

        <label className={labelClass + " mt-6"}>
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
          <div className="mt-5 space-y-3 border border-signal/25 bg-signal/10 p-4 text-sm leading-6 text-mist/90">
            <p>
              Issues a 1-year leaf certificate signed by this installation&apos;s
              private CA. Download the CA once and trust it on your PC —
              renewals keep working without reinstalling.
            </p>
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
              The certificate is checked for expiry, SAN coverage, and a matching
              private key before it is installed.
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
                      event.target.value === "dns-01" ? "dns-01" : "http-01",
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
                  <option value="ecdsa">ECDSA P-256 · modern clients</option>
                </select>
              </label>
            </div>
            {challenge === "http-01" ? (
              <p className="border border-link/25 bg-link/10 p-4 text-xs leading-5 text-link">
                Point the domain to this installation and make port 80 reachable
                from the Internet while Let&apos;s Encrypt checks the challenge.
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
                  domain. The token is encrypted before persistence and is only
                  used for <code>_acme-challenge</code> TXT records.
                </p>
              </div>
            )}
            {hasWildcard && challenge === "http-01" ? (
              <p className="border border-signal/30 bg-signal/10 p-3 text-xs text-signal">
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
                    <option value="staging">Staging · safe for tests</option>
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

        <button
          type="submit"
          disabled={submitting}
          className="pc-btn mt-6"
        >
          {submitLabel}
        </button>
      </form>
    </div>,
    document.body,
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
      className={`rounded-none px-3 py-2 text-xs transition ${
        props.active
          ? "bg-signal font-semibold text-[#0A0C0F]"
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
        className="mt-2 block w-full rounded-none border border-dashed border-line bg-bay px-3 py-3 text-xs text-mute file:mr-3 file:rounded-none file:border-0 file:bg-raised file:px-2 file:py-1.5 file:text-xs file:text-mist"
      />
    </label>
  );
}
