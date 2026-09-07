import { useState } from "react";
import { CertificateRequestDialog } from "./CertificateRequestDialog";

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
  const [dialogOpen, setDialogOpen] = useState(false);
  const [downloadingCA, setDownloadingCA] = useState(false);
  const [regeneratingId, setRegeneratingId] = useState<string>();
  const [deletingId, setDeletingId] = useState<string>();

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
        props.onError(
          payload.error ?? "Could not download trust certificate",
        );
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

  async function regenerateSelfSigned(certificateId: string) {
    props.onMessage("");
    props.onError("");
    setRegeneratingId(certificateId);
    try {
      const response = await fetch(
        `/api/certificates/${encodeURIComponent(certificateId)}/renew`,
        { method: "POST", credentials: "include" },
      );
      const payload = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) {
        props.onError(
          payload.error ?? "Could not regenerate certificate",
        );
        await props.onRefresh();
        return;
      }
      props.onMessage(
        "Internal certificate regenerated — apply is queued if an owner exists",
      );
      await props.onRefresh();
    } catch {
      props.onError("Could not regenerate certificate");
    } finally {
      setRegeneratingId(undefined);
    }
  }

  async function deleteCertificate(certificate: DashboardCertificate) {
    const label = certificate.hostnames.join(", ") || certificate.id;
    if (
      !window.confirm(
        `Delete certificate for ${label}? This cannot be undone.`,
      )
    ) {
      return;
    }
    props.onMessage("");
    props.onError("");
    setDeletingId(certificate.id);
    try {
      const response = await fetch(
        `/api/certificates/${encodeURIComponent(certificate.id)}`,
        { method: "DELETE", credentials: "include" },
      );
      const payload = (await response.json().catch(() => ({}))) as {
        error?: string;
      };
      if (!response.ok) {
        props.onError(payload.error ?? "Could not delete certificate");
        return;
      }
      props.onMessage("Certificate deleted");
      await props.onRefresh();
    } catch {
      props.onError("Could not delete certificate");
    } finally {
      setDeletingId(undefined);
    }
  }

  return (
    <div className="mt-8 space-y-6">
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
              className="pc-btn"
              onClick={() => setDialogOpen(true)}
            >
              Request certificate
            </button>
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
              <CertificateCard
                key={certificate.id}
                certificate={certificate}
                regenerating={regeneratingId === certificate.id}
                deleting={deletingId === certificate.id}
                onRegenerate={
                  certificate.issuer === "self-signed" &&
                  certificate.status === "active"
                    ? () => void regenerateSelfSigned(certificate.id)
                    : undefined
                }
                onDelete={() => void deleteCertificate(certificate)}
              />
            ))
          ) : (
            <p className="rounded-xl border border-dashed border-line p-5 text-sm text-faint">
              No certificates yet. Generate an internal certificate or request one
              from a public certificate authority.
            </p>
          )}
        </div>
      </section>

      <CertificateRequestDialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onSuccess={props.onMessage}
        onError={props.onError}
        onRefresh={props.onRefresh}
      />
    </div>
  );
}

function CertificateCard(props: {
  certificate: DashboardCertificate;
  regenerating?: boolean;
  deleting?: boolean;
  onRegenerate?: () => void;
  onDelete?: () => void;
}) {
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
  const busy = Boolean(props.regenerating || props.deleting);
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
      <div className="flex flex-col items-start gap-2 md:items-end">
        <p className={`text-sm font-medium ${statusTone}`}>
          {certificate.status}
        </p>
        <p className="text-xs text-faint">
          {expires
            ? daysRemaining !== undefined && daysRemaining >= 0
              ? `${daysRemaining} days left · ${expires.toLocaleDateString()}`
              : `Expired · ${expires.toLocaleDateString()}`
            : "Expiry pending"}
        </p>
        <div className="flex flex-wrap gap-2">
          {props.onRegenerate ? (
            <button
              type="button"
              className="pc-btn-ghost !text-xs"
              disabled={busy}
              onClick={props.onRegenerate}
            >
              {props.regenerating ? "Regenerating…" : "Regenerate"}
            </button>
          ) : null}
          {props.onDelete ? (
            <button
              type="button"
              className="pc-btn-ghost !text-xs !text-danger"
              disabled={busy}
              onClick={props.onDelete}
            >
              {props.deleting ? "Deleting…" : "Delete"}
            </button>
          ) : null}
        </div>
      </div>
    </article>
  );
}
