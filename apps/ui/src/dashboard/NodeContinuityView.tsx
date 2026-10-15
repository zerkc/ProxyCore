import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, confirmEnrollment, draftEnrollment, EnrollmentApiError, recoverEnrollment } from "../api";
import type { ConfirmResult, DraftPreview } from "../types";

type NodeState =
  | { kind: "recovering" }
  | { kind: "entry"; error?: string; recoveryPrompt: boolean }
  | { kind: "preview"; draft: DraftPreview; error?: string }
  | { kind: "pending" }
  | { kind: "committed"; result: ConfirmResult };

export function NodeContinuityView() {
  const [state, setState] = useState<NodeState>({ kind: "recovering" });
  const [token, setToken] = useState("");
  const [primaryURL, setPrimaryURL] = useState("");
  // The CA is public certificate material, so it survives a failed attempt and
  // the owner does not have to load it again after fixing the token or URL.
  const [caPem, setCaPem] = useState("");
  const activeRequest = useRef<AbortController | undefined>(undefined);
  const mounted = useRef(true);

  useEffect(() => {
    const controller = new AbortController();
    activeRequest.current = controller;
    let cancelled = false;
    mounted.current = true;
    void recoverEnrollment(undefined, controller.signal).then((result) => {
      if (cancelled || !mounted.current) return;
      setState(result === "token-missing" ? { kind: "entry", recoveryPrompt: true } : { kind: "preview", draft: sanitizeDraft(result) });
    }).catch(() => {
      if (!cancelled && !controller.signal.aborted && mounted.current) {
        setState({ kind: "entry", recoveryPrompt: true, error: "Enrollment recovery is unavailable. Please re-enter your enrollment token." });
      }
    }).finally(() => {
      if (activeRequest.current === controller) activeRequest.current = undefined;
    });
    return () => {
      cancelled = true;
      mounted.current = false;
      controller.abort();
      activeRequest.current?.abort();
    };
  }, []);

  async function submitDraft(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const selfContained = isSelfContainedToken(token);
    if (!token.trim() || (!selfContained && !primaryURL.trim())) {
      setState({ kind: "entry", recoveryPrompt: state.kind === "entry" && state.recoveryPrompt, error: !token.trim() ? "Enter the enrollment token." : "Enter the PRIMARY URL for this legacy token." });
      return;
    }
    const submittedToken = token;
    const controller = replaceActiveRequest();
    setState({ kind: "recovering" });
    try {
      const draft = await draftEnrollment(submittedToken, primaryURL.trim() || undefined, controller.signal, caPem.trim() || undefined);
      if (!mounted.current || controller.signal.aborted) return;
      setToken("");
      setState({ kind: "preview", draft: sanitizeDraft(draft) });
    } catch (caught) {
      if (mounted.current && !controller.signal.aborted) setState({ kind: "entry", recoveryPrompt: false, error: draftError(caught) });
    } finally { clearActiveRequest(controller); }
  }

  function cancelPreview() {
    if (state.kind !== "preview") return;
    setToken("");
    setState({ kind: "entry", recoveryPrompt: false });
  }

  async function confirmDraft() {
    if (state.kind !== "preview") return;
    const draft = state.draft;
    const controller = replaceActiveRequest();
    setState({ kind: "pending" });
    try {
      const result = await confirmEnrollment(draft.draftId, controller.signal);
      if (!mounted.current || controller.signal.aborted) return;
      setState({ kind: "committed", result: sanitizeConfirmResult(result) });
    } catch (caught) {
      if (mounted.current && !controller.signal.aborted) setState({ kind: "preview", draft, error: confirmError(caught) });
    } finally { clearActiveRequest(controller); }
  }

  if (state.kind === "recovering") return <section className="mt-8 space-y-5" aria-labelledby="node-continuity-title"><NodeHeader /><p className="border-y border-dashed border-line py-5 text-sm text-faint" role="status">Checking for a recoverable enrollment draft…</p></section>;
  if (state.kind === "entry") return <EntrySurface error={state.error} recoveryPrompt={state.recoveryPrompt} token={token} primaryURL={primaryURL} caPem={caPem} onTokenChange={setToken} onPrimaryURLChange={setPrimaryURL} onCaChange={setCaPem} onSubmit={submitDraft} />;
  if (state.kind === "preview") return <PreviewSurface draft={state.draft} error={state.error} onConfirm={() => void confirmDraft()} onCancel={cancelPreview} />;
  if (state.kind === "pending") return <PendingSurface />;
  return <CommittedSurface result={state.result} />;

  function replaceActiveRequest() {
    activeRequest.current?.abort();
    const controller = new AbortController();
    activeRequest.current = controller;
    return controller;
  }
  function clearActiveRequest(controller: AbortController) {
    if (activeRequest.current === controller) activeRequest.current = undefined;
  }
}

function NodeHeader() {
  return <header className="border-b border-line/80 pb-5"><p className="pc-eyebrow pc-eyebrow-signal">node continuity</p><h2 id="node-continuity-title" className="pc-title mt-2 text-2xl text-mist">Connect this NODE</h2><p className="mt-3 max-w-2xl text-sm leading-6 text-mute">Review the authenticated PRIMARY snapshot before this installation becomes a NODE.</p></header>;
}

function isSelfContainedToken(token: string) { return token.trim().startsWith("pcenr2_"); }

const MAX_CA_FILE_BYTES = 64 * 1024;

function EntrySurface({ error, recoveryPrompt, token, primaryURL, caPem, onTokenChange, onPrimaryURLChange, onCaChange, onSubmit }: {
  error?: string; recoveryPrompt: boolean; token: string; primaryURL: string; caPem: string;
  onTokenChange: (value: string) => void; onPrimaryURLChange: (value: string) => void; onCaChange: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  async function loadCaFile(event: { currentTarget: { files?: ArrayLike<File> | null } }) {
    const file = event.currentTarget.files?.[0];
    if (!file || file.size > MAX_CA_FILE_BYTES) return;
    onCaChange(await file.text());
  }
  const selfContained = isSelfContainedToken(token);
  return <section className="mt-8 space-y-5" aria-labelledby="node-continuity-title">
    <NodeHeader />
    <p className="border-l-2 border-signal/70 pl-3 text-sm leading-6 text-mute">{recoveryPrompt ? "Please re-enter your enrollment token. Tokens are not stored on a NODE." : "Paste your enrollment token to start a new preview. A current token already carries the PRIMARY address and certificate, so nothing else is needed."}</p>
    {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
    <form className="space-y-5" data-testid="node-enrollment-form" onSubmit={onSubmit}>
      <label className="pc-label" htmlFor="node-enrollment-token">Enrollment token
        <textarea id="node-enrollment-token" className="pc-input min-h-24 resize-y break-all font-mono" data-testid="node-enrollment-token" value={token} onInput={(event) => onTokenChange(event.currentTarget.value)} onChange={(event) => onTokenChange(event.currentTarget.value)} autoComplete="off" spellCheck={false} />
      </label>
      {selfContained || !token.trim() ? null : <p className="text-xs text-faint" data-testid="node-legacy-token-notice">This looks like a legacy token. Enter the PRIMARY URL below; add the CA certificate if the PRIMARY uses a private CA.</p>}
      <details className="space-y-5 border-y border-line/80 py-3" data-testid="node-advanced-token" open={!selfContained}>
        <summary className="cursor-pointer text-sm text-mute">Advanced / legacy token</summary>
        <label className="pc-label mt-4 block" htmlFor="node-primary-url">PRIMARY URL{selfContained ? " (optional)" : ""}
          <input id="node-primary-url" className="pc-input font-mono" data-testid="node-primary-url" value={primaryURL} onInput={(event) => onPrimaryURLChange(event.currentTarget.value)} onChange={(event) => onPrimaryURLChange(event.currentTarget.value)} placeholder="https://primary.example:3443/" aria-describedby="node-primary-url-help" autoComplete="off" spellCheck={false} />
          <span id="node-primary-url-help" className="mt-1 block text-xs text-faint" data-testid="node-primary-url-help">{selfContained ? "Not needed for this token. If set, it must match the address inside the token." : "Use the form https://<host>:<port>/ with an explicit port and a trailing slash."}</span>
        </label>
        <label className="pc-label mt-4 block" htmlFor="node-primary-ca">PRIMARY CA certificate (optional)
          <textarea id="node-primary-ca" className="pc-input min-h-24 resize-y font-mono" data-testid="node-primary-ca" value={caPem} onInput={(event) => onCaChange(event.currentTarget.value)} onChange={(event) => onCaChange(event.currentTarget.value)} placeholder="-----BEGIN CERTIFICATE-----" aria-describedby="node-primary-ca-help" autoComplete="off" spellCheck={false} />
          <span id="node-primary-ca-help" className="mt-1 block text-xs text-faint" data-testid="node-primary-ca-help">Needed for legacy tokens when the PRIMARY uses a self-signed or private CA. Download it from the PRIMARY's enrollment trust panel, then paste it or load the file. It is trusted for this PRIMARY only, and loading a new one replaces the previous CA for the same address.</span>
        </label>
        <input type="file" accept=".pem,.crt,.cer" className="pc-input mt-4" data-testid="node-primary-ca-file" aria-label="Load PRIMARY CA certificate file" onChange={(event) => void loadCaFile(event)} />
      </details>
      <button type="submit" className="pc-btn" data-testid="node-draft-submit">Preview enrollment</button>
    </form>
  </section>;
}

function PreviewSurface({ draft, error, onConfirm, onCancel }: { draft: DraftPreview; error?: string; onConfirm: () => void; onCancel: () => void }) {
  const preview = draft.envelopePreview;
  const fields = [["ingress", formatIngress(preview.ingress)], ["role", preview.role], ["generation", String(preview.generation)], ["cluster key id", preview.clusterKeyId ?? "—"], ["content hash", preview.contentHash], ...(draft.primaryCaFingerprint ? [["primary ca fingerprint", draft.primaryCaFingerprint]] : []), ["draft expires", formatDate(draft.expiresAt)]];
  return <section className="mt-8 space-y-5" aria-labelledby="node-preview-title">
    <header className="border-b border-line/80 pb-5"><p className="pc-eyebrow pc-eyebrow-signal">node continuity / review</p><h2 id="node-preview-title" className="pc-title mt-2 text-2xl text-mist">Enrollment preview</h2><p className="mt-3 max-w-2xl text-sm leading-6 text-mute">The following redacted snapshot summary will be applied after confirmation.</p></header>
    {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
    <dl className="grid gap-4 border-y border-line/80 px-1 py-5 sm:grid-cols-2">{fields.map(([label, value]) => <PreviewField key={label} label={label} value={value} />)}</dl>
    <div className="flex flex-wrap gap-2"><button type="button" className="pc-btn" data-testid="node-confirm" onClick={onConfirm}>Confirm</button><button type="button" className="pc-btn-ghost" data-testid="node-cancel" onClick={onCancel}>Cancel</button></div>
  </section>;
}

function PendingSurface() {
  return <section className="mt-8 space-y-5" aria-labelledby="node-pending-title"><header className="border-b border-line/80 pb-5"><p className="pc-eyebrow pc-eyebrow-signal">node continuity / apply</p><h2 id="node-pending-title" className="pc-title mt-2 text-2xl text-mist">Applying enrollment</h2></header><p className="border-y border-dashed border-line py-5 text-sm text-faint" role="status">Applying the verified snapshot and committing NODE identity…</p></section>;
}

function CommittedSurface({ result }: { result: ConfirmResult }) {
  const fields = [["role", result.role], ["generation", String(result.generation)], ["node id", result.nodeId], ["cluster key id", result.clusterKeyId ?? "—"], ["archive id", result.archiveId], ["apply job id", result.applyJobId]];
  function downloadAudit() {
    downloadText(JSON.stringify({ role: result.role, generation: result.generation, nodeId: result.nodeId, ...(result.clusterKeyId ? { clusterKeyId: result.clusterKeyId } : {}), archiveId: result.archiveId, applyJobId: result.applyJobId }, null, 2), "proxycore-node-enrollment-audit.json");
  }
  return <section className="mt-8 space-y-5" aria-labelledby="node-committed-title">
    <header className="border-b border-line/80 pb-5"><p className="pc-eyebrow pc-eyebrow-signal">node continuity / committed</p><h2 id="node-committed-title" className="pc-title mt-2 text-2xl text-mist">Enrollment committed</h2><p className="mt-3 max-w-2xl text-sm leading-6 text-mute">This installation is now a NODE. Only the post-conversion identity is shown.</p></header>
    <dl className="grid gap-4 border-y border-line/80 px-1 py-5 sm:grid-cols-2">{fields.map(([label, value]) => <PreviewField key={label} label={label} value={value} />)}</dl>
    <button type="button" className="pc-btn-ghost" data-testid="node-audit-download" onClick={downloadAudit}>Download post-conversion audit info</button>
  </section>;
}

function PreviewField({ label, value }: { label: string; value: string }) {
  return <div><dt className="pc-eyebrow">{label}</dt><dd className="mt-1 break-all font-mono text-sm text-mist">{value}</dd></div>;
}

function sanitizeDraft(draft: DraftPreview): DraftPreview {
  const envelope = draft.envelopePreview;
  const overlay = draft.nodeLocalOverlay ? { nodeId: draft.nodeLocalOverlay.nodeId, role: draft.nodeLocalOverlay.role, ingress: { ipv4: draft.nodeLocalOverlay.ingress?.ipv4, ipv6: draft.nodeLocalOverlay.ingress?.ipv6 } } : undefined;
  return { draftId: draft.draftId, envelopePreview: { ingress: { ipv4: envelope.ingress?.ipv4, ipv6: envelope.ingress?.ipv6 }, role: envelope.role, generation: envelope.generation, ...(envelope.clusterKeyId ? { clusterKeyId: envelope.clusterKeyId } : {}), contentHash: envelope.contentHash }, ...(overlay ? { nodeLocalOverlay: overlay } : {}), expiresAt: draft.expiresAt, ...(typeof draft.primaryCaFingerprint === "string" && /^[0-9a-f]{64}$/.test(draft.primaryCaFingerprint) ? { primaryCaFingerprint: draft.primaryCaFingerprint } : {}) };
}

function sanitizeConfirmResult(result: ConfirmResult): ConfirmResult {
  return { role: result.role, generation: result.generation, nodeId: result.nodeId, ...(result.clusterKeyId ? { clusterKeyId: result.clusterKeyId } : {}), archiveId: result.archiveId, applyJobId: result.applyJobId };
}
function formatIngress(ingress: DraftPreview["envelopePreview"]["ingress"]) { return [ingress.ipv4, ingress.ipv6].filter(Boolean).join(" / ") || "—"; }
function formatDate(value: string) { const timestamp = Date.parse(value); return Number.isNaN(timestamp) ? "—" : new Date(timestamp).toISOString(); }
const PRIMARY_URL_HINT = "The enrollment request was rejected. The PRIMARY URL must look like https://<host>:<port>/ with an explicit port and a trailing slash.";
function draftError(caught: unknown) {
  if (caught instanceof EnrollmentApiError && caught.kind === "untrusted-certificate") return "The PRIMARY certificate is not trusted by this NODE. Download the CA from the PRIMARY's enrollment trust panel and load it below, then try again.";
  if (caught instanceof EnrollmentApiError && caught.kind === "invalid-ca") return "The CA certificate is not valid. Load the PEM file downloaded from the PRIMARY's enrollment trust panel.";
  if (caught instanceof ApiError && caught.status === 403) return "The enrollment token was rejected. Check the token and try again.";
  if (caught instanceof EnrollmentApiError && caught.status === 400) return PRIMARY_URL_HINT;
  if (caught instanceof EnrollmentApiError && caught.status === 410) return "The enrollment token has expired or been revoked. Enter a current token.";
  if (caught instanceof EnrollmentApiError && caught.status >= 500) return "The enrollment preview is temporarily unavailable. Try again.";
  return "The enrollment preview could not be loaded. Check the URL and try again.";
}
function confirmError(caught: unknown) {
  if (caught instanceof EnrollmentApiError && [409, 410].includes(caught.status)) return "Enrollment confirmation was rejected. Review the preview and try again.";
  if (caught instanceof EnrollmentApiError && caught.status >= 500) return "Enrollment confirmation is temporarily unavailable. Try again.";
  return "Enrollment confirmation could not be completed. Review the preview and try again.";
}
function downloadText(value: string, filename: string) {
  const url = URL.createObjectURL(new Blob([value], { type: "application/json" }));
  try {
    const anchor = document.createElement("a");
    anchor.href = url; anchor.download = filename; anchor.setAttribute("data-testid", "node-audit-download-link");
    document.body.appendChild(anchor);
    try { anchor.click(); } finally { anchor.remove(); }
  } finally { URL.revokeObjectURL(url); }
}
