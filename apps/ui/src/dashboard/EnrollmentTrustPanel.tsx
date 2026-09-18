import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ApiError,
  downloadEnrollmentTrustCA,
  getEnrollmentTrust,
} from "../api";
import type { EnrollmentTrust, TopologyIdentity } from "./types";

const ENROLLMENT_TRUST_ROLES = new Set<TopologyIdentity["role"]>([
  "standalone-primary",
  "primary",
  "primary-with-nodes",
]);

type TrustState =
  | { kind: "loading" }
  | { kind: "data"; trust: EnrollmentTrust }
  | { kind: "forbidden" }
  | { kind: "error"; message: string };

export function canViewEnrollmentTrust(identity?: TopologyIdentity) {
  return Boolean(
    identity &&
      !identity.stalePrimary &&
      ENROLLMENT_TRUST_ROLES.has(identity.role),
  );
}

export function EnrollmentTrustPanel({
  identity,
}: {
  identity?: TopologyIdentity;
}) {
  const navigate = useNavigate();
  const [reloadToken, setReloadToken] = useState(0);
  const [trustState, setTrustState] = useState<TrustState>({ kind: "loading" });
  const [copying, setCopying] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [copyError, setCopyError] = useState("");
  const [downloadError, setDownloadError] = useState("");
  const copyInFlight = useRef(false);
  const downloadInFlight = useRef(false);
  const feedbackTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => {
    if (!canViewEnrollmentTrust(identity)) return;

    const controller = new AbortController();
    let cancelled = false;
    setTrustState({ kind: "loading" });
    setFeedback("");
    setCopyError("");
    setDownloadError("");

    async function load() {
      try {
        const loaded = await getEnrollmentTrust(controller.signal);
        if (cancelled) return;
        setTrustState({ kind: "data", trust: loaded });
      } catch (caught) {
        if (cancelled || controller.signal.aborted) return;
        if (caught instanceof ApiError && caught.status === 401) {
          navigate("/login");
          return;
        }
        if (caught instanceof ApiError && caught.status === 403) {
          setTrustState({ kind: "forbidden" });
          return;
        }
        setTrustState({
          kind: "error",
          message: trustLoadError(caught),
        });
      }
    }

    void load();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [
    identity?.installationId,
    identity?.nodeId,
    identity?.role,
    identity?.leadershipGeneration,
    identity?.latestKnownGeneration,
    identity?.stalePrimary,
    identity?.writable,
    navigate,
    reloadToken,
  ]);

  useEffect(() => {
    return () => {
      if (feedbackTimer.current) clearTimeout(feedbackTimer.current);
    };
  }, []);

  if (!canViewEnrollmentTrust(identity)) return null;

  const trust = trustState.kind === "data" ? trustState.trust : undefined;
  const fingerprint = trust?.status === "ready" && trust.ready
    ? canonicalFingerprint(trust.caDerSha256)
    : undefined;

  function retry() {
    setReloadToken((token) => token + 1);
  }

  async function copyFingerprint() {
    if (!fingerprint || copyInFlight.current) return;
    copyInFlight.current = true;
    setCopying(true);
    setFeedback("");
    setCopyError("");
    try {
      await copyText(fingerprint);
      showFeedback("Fingerprint copied to clipboard.");
    } catch (caught) {
      setCopyError(copyErrorMessage(caught));
    } finally {
      copyInFlight.current = false;
      setCopying(false);
    }
  }

  async function downloadCA() {
    if (!fingerprint || downloadInFlight.current) return;
    downloadInFlight.current = true;
    setDownloading(true);
    setFeedback("");
    setCopyError("");
    setDownloadError("");
    try {
      const result = await downloadEnrollmentTrustCA();
      downloadBlob(result.blob, result.filename);
      showFeedback(`Downloaded ${result.filename}.`);
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 401) {
        navigate("/login");
        return;
      }
      if (caught instanceof ApiError && caught.status === 403) {
        setTrustState({ kind: "forbidden" });
        return;
      }
      setDownloadError(downloadErrorMessage(caught));
    } finally {
      downloadInFlight.current = false;
      setDownloading(false);
    }
  }

  return (
    <section className="mt-8 space-y-5 border-t border-line/80 pt-6" aria-labelledby="enrollment-trust-title">
      <header className="border-b border-line/80 pb-5">
        <p className="pc-eyebrow pc-eyebrow-signal">owner trust</p>
        <h2 id="enrollment-trust-title" className="pc-title mt-2 text-2xl text-mist">
          Enrollment trust
        </h2>
        <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
          Confirm this PRIMARY&apos;s enrollment CA before trusting a NODE connection.
        </p>
      </header>

      {trustState.kind === "loading" ? (
        <p className="border-y border-dashed border-line py-5 text-sm text-faint" role="status">
          Loading enrollment trust…
        </p>
      ) : trustState.kind === "forbidden" ? (
        <p className="pc-toast-err" role="alert">
          Owner access is required to view enrollment trust.
        </p>
      ) : trustState.kind === "error" ? (
        <div className="space-y-3">
          <p className="pc-toast-err" role="alert">
            {trustState.message}
          </p>
          <button type="button" className="pc-btn-ghost !text-xs" onClick={retry}>
            Retry trust load
          </button>
        </div>
      ) : trust?.status === "unconfigured" ? (
        <TrustStatus
          title="Unconfigured"
          description="Configure enrollment SANs above before the PRIMARY can establish trust material."
        />
      ) : trust?.status === "not-ready" ? (
        <div className="space-y-4">
          <TrustStatus
            title="Enrollment trust is not ready"
            description="Trust material activates with the PRIMARY enrollment runtime. Retry after the runtime is active."
          />
          <button type="button" className="pc-btn-ghost !text-xs" onClick={retry}>
            Retry trust load
          </button>
        </div>
      ) : fingerprint ? (
        <div className="space-y-5">
          <div className="flex flex-wrap items-start justify-between gap-4 border-y border-line/80 px-1 py-4">
            <div>
              <p className="pc-eyebrow">trust status</p>
              <h3 className="pc-title mt-2 text-xl text-mist">Ready</h3>
            </div>
            <span className="font-mono text-xs uppercase tracking-[0.1em] text-ok">
              active
            </span>
          </div>

          <div className="space-y-2">
            <p className="text-xs uppercase tracking-[0.1em] text-faint">
              SHA-256 fingerprint
            </p>
            <code
              className="block break-all font-mono text-sm leading-7 text-mist"
              aria-label={`SHA-256 fingerprint ${fingerprint}`}
              data-fingerprint={fingerprint}
            >
              {groupFingerprint(fingerprint)}
            </code>
          </div>

          <p className="text-sm leading-6 text-mute">
            Leaf certificate expires {formatExpiry(trust?.expiresAt)}.
          </p>

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              className="pc-btn"
              disabled={copying || downloading}
              aria-label="Copy enrollment trust SHA-256 fingerprint"
              onClick={() => void copyFingerprint()}
            >
              {copying ? "Copying…" : "Copy fingerprint"}
            </button>
            <button
              type="button"
              className="pc-btn-ghost"
              disabled={downloading || copying}
              aria-label="Download enrollment trust CA"
              onClick={() => void downloadCA()}
            >
              {downloading ? "Preparing…" : "Download CA"}
            </button>
          </div>

          {feedback ? (
            <p className="pc-toast-ok" role="status" aria-live="polite">
              {feedback}
            </p>
          ) : null}
          {copyError ? (
            <p className="pc-toast-err" role="alert">
              {copyError}
            </p>
          ) : null}
          {downloadError ? (
            <p className="pc-toast-err" role="alert">
              {downloadError}
            </p>
          ) : null}
        </div>
      ) : (
        <div className="space-y-3">
          <p className="pc-toast-err" role="alert">
            Enrollment trust is ready but its fingerprint is unavailable.
          </p>
          <button type="button" className="pc-btn-ghost !text-xs" onClick={retry}>
            Retry trust load
          </button>
        </div>
      )}
    </section>
  );

  function showFeedback(message: string) {
    if (feedbackTimer.current) clearTimeout(feedbackTimer.current);
    setFeedback(message);
    feedbackTimer.current = setTimeout(() => setFeedback(""), 4_000);
  }
}

function TrustStatus({ title, description }: { title: string; description: string }) {
  return (
    <div className="border-y border-line/80 px-1 py-4">
      <p className="pc-eyebrow">trust status</p>
      <h3 className="pc-title mt-2 text-xl text-mist">{title}</h3>
      <p className="mt-3 text-sm leading-6 text-mute">{description}</p>
    </div>
  );
}

function canonicalFingerprint(value?: string) {
  const normalized = value?.toLowerCase();
  return normalized && /^[0-9a-f]{64}$/.test(normalized) ? normalized : undefined;
}

function groupFingerprint(value: string) {
  return value.match(/.{1,4}/g)?.join(" ") ?? value;
}

function formatExpiry(value?: string) {
  if (!value) return "an unavailable date";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "an unavailable date";
  return new Intl.DateTimeFormat(undefined, { dateStyle: "long" }).format(date);
}

async function copyText(value: string) {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      // Fall through to the legacy copy command when the permission prompt rejects.
    }
  }

  const previousFocus = document.activeElement;
  let textarea: HTMLTextAreaElement | undefined;
  try {
    textarea = document.createElement("textarea");
    textarea.value = value;
    textarea.setAttribute("readonly", "true");
    textarea.setAttribute("aria-hidden", "true");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select?.();
    const copied =
      typeof document.execCommand === "function" && document.execCommand("copy");
    if (!copied) throw new Error("clipboard unavailable");
  } finally {
    textarea?.remove();
    if (previousFocus instanceof HTMLElement) previousFocus.focus();
  }
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  try {
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = filename;
    anchor.setAttribute("aria-label", `Download ${filename}`);
    document.body.appendChild(anchor);
    try {
      anchor.click();
    } finally {
      anchor.remove();
    }
  } finally {
    URL.revokeObjectURL(url);
  }
}

function trustLoadError(caught: unknown) {
  if (caught instanceof ApiError) {
    if (caught.status === 409) {
      return "Enrollment trust is not ready. Retry trust load.";
    }
    if ([500, 502, 503, 504].includes(caught.status)) {
      return "Enrollment trust is temporarily unavailable. Retry trust load.";
    }
    if (caught.status === 400) {
      return "Enrollment trust configuration is invalid. Review the enrollment SANs.";
    }
  }
  return "Enrollment trust could not be loaded. Retry trust load.";
}

function copyErrorMessage(caught: unknown) {
  if (caught instanceof ApiError && caught.status === 403) {
    return "Owner access is required to copy the enrollment fingerprint.";
  }
  return "Could not copy fingerprint. Copy the value manually.";
}

function downloadErrorMessage(caught: unknown) {
  if (caught instanceof ApiError) {
    if (caught.status === 409) {
      return "Enrollment CA is not ready. Try again after enrollment trust is ready.";
    }
    if ([500, 502, 503, 504].includes(caught.status)) {
      return "Enrollment CA is temporarily unavailable. Try again.";
    }
    if (caught.status === 400) {
      return "Enrollment CA download is unavailable. Try again.";
    }
  }
  return "Could not download enrollment CA. Try again.";
}
