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
    if (!token.trim() || !primaryURL.trim()) {
      setState({ kind: "entry", recoveryPrompt: state.kind === "entry" && state.recoveryPrompt, error: "Enter the enrollment token and PRIMARY URL." });
      return;
    }
    const submittedToken = token;
    const controller = replaceActiveRequest();
    setState({ kind: "recovering" });
    try {
      const draft = await draftEnrollment(submittedToken, primaryURL.trim(), controller.signal);
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
  if (state.kind === "entry") return <EntrySurface error={state.error} recoveryPrompt={state.recoveryPrompt} token={token} primaryURL={primaryURL} onTokenChange={setToken} onPrimaryURLChange={setPrimaryURL} onSubmit={submitDraft} />;
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

function EntrySurface({ error, recoveryPrompt, token, primaryURL, onTokenChange, onPrimaryURLChange, onSubmit }: {
  error?: string; recoveryPrompt: boolean; token: string; primaryURL: string;
  onTokenChange: (value: string) => void; onPrimaryURLChange: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  return <section className="mt-8 space-y-5" aria-labelledby="node-continuity-title">
    <NodeHeader />
    <p className="border-l-2 border-signal/70 pl-3 text-sm leading-6 text-mute">{recoveryPrompt ? "Please re-enter your enrollment token. Tokens are not stored on a NODE." : "Paste your enrollment token and PRIMARY URL to start a new preview."}</p>
    {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
    <form className="space-y-5" data-testid="node-enrollment-form" onSubmit={onSubmit}>
      <label className="pc-label" htmlFor="node-enrollment-token">Enrollment token
        <textarea id="node-enrollment-token" className="pc-input min-h-24 resize-y font-mono" data-testid="node-enrollment-token" value={token} onInput={(event) => onTokenChange(event.currentTarget.value)} onChange={(event) => onTokenChange(event.currentTarget.value)} autoComplete="off" spellCheck={false} />
      </label>
      <label className="pc-label" htmlFor="node-primary-url">PRIMARY URL
        <input id="node-primary-url" className="pc-input font-mono" data-testid="node-primary-url" value={primaryURL} onInput={(event) => onPrimaryURLChange(event.currentTarget.value)} onChange={(event) => onPrimaryURLChange(event.currentTarget.value)} placeholder="https://primary.example" autoComplete="off" spellCheck={false} />
      </label>
      <button type="submit" className="pc-btn" data-testid="node-draft-submit">Preview enrollment</button>
    </form>
  </section>;
}

function PreviewSurface({ draft, error, onConfirm, onCancel }: { draft: DraftPreview; error?: string; onConfirm: () => void; onCancel: () => void }) {
  const preview = draft.envelopePreview;
  const fields = [["ingress", formatIngress(preview.ingress)], ["role", preview.role], ["generation", String(preview.generation)], ["cluster key id", preview.clusterKeyId ?? "—"], ["content hash", preview.contentHash], ["draft expires", formatDate(draft.expiresAt)]];
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
  return { draftId: draft.draftId, envelopePreview: { ingress: { ipv4: envelope.ingress?.ipv4, ipv6: envelope.ingress?.ipv6 }, role: envelope.role, generation: envelope.generation, ...(envelope.clusterKeyId ? { clusterKeyId: envelope.clusterKeyId } : {}), contentHash: envelope.contentHash }, ...(overlay ? { nodeLocalOverlay: overlay } : {}), expiresAt: draft.expiresAt };
}

function sanitizeConfirmResult(result: ConfirmResult): ConfirmResult {
  return { role: result.role, generation: result.generation, nodeId: result.nodeId, ...(result.clusterKeyId ? { clusterKeyId: result.clusterKeyId } : {}), archiveId: result.archiveId, applyJobId: result.applyJobId };
}
function formatIngress(ingress: DraftPreview["envelopePreview"]["ingress"]) { return [ingress.ipv4, ingress.ipv6].filter(Boolean).join(" / ") || "—"; }
function formatDate(value: string) { const timestamp = Date.parse(value); return Number.isNaN(timestamp) ? "—" : new Date(timestamp).toISOString(); }
function draftError(caught: unknown) {
  if (caught instanceof ApiError && caught.status === 403) return "The enrollment token was rejected. Check the token and try again.";
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
