import { useEffect, useRef, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import {
  ApiError,
  createEnrollmentToken,
  EnrollmentApiError,
  listEnrollmentTokens,
  revokeEnrollmentToken,
} from "../api";
import type { EnrollmentTokenCreation, EnrollmentTokenSummary } from "../types";
import type { TopologyIdentity } from "./types";

const REVOKE_CONFIRMATION = "replace-and-revoke";
const TOKEN_FILENAME = "proxycore-enrollment-token.txt";
const TOKEN_ROLES = new Set<TopologyIdentity["role"]>([
  "standalone-primary",
  "primary",
  "primary-with-nodes",
]);

export function canManageEnrollmentTokens(identity?: TopologyIdentity) {
  return Boolean(!identity || (!identity.stalePrimary && TOKEN_ROLES.has(identity.role)));
}

export function EnrollmentTokenPanel({ identity }: { identity?: TopologyIdentity }) {
  const navigate = useNavigate();
  const tokenValueRef = useRef<HTMLTextAreaElement>(null);
  const actionRequest = useRef<AbortController | undefined>(undefined);
  const clusterKeyID = (identity as (TopologyIdentity & { clusterKeyId?: string }) | undefined)?.clusterKeyId;
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState("");
  const [tokens, setTokens] = useState<EnrollmentTokenSummary[]>([]);
  const [creation, setCreation] = useState<EnrollmentTokenCreation>();
  const [creating, setCreating] = useState(false);
  const [revokeID, setRevokeID] = useState<string>();
  const [revokeConfirmation, setRevokeConfirmation] = useState("");
  const [revoking, setRevoking] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!canManageEnrollmentTokens(identity)) return;
    const controller = new AbortController();
    let cancelled = false;
    setLoading(true);
    setListError("");
    setTokens([]);
    setCreation(undefined);
    setRevokeID(undefined);
    setRevokeConfirmation("");
    setFeedback("");
    setError("");

    async function load() {
      try {
        const loaded = await listEnrollmentTokens(controller.signal);
        if (cancelled) return;
        setTokens(loaded.map(sanitizeTokenSummary));
      } catch (caught) {
        if (cancelled || controller.signal.aborted) return;
        if (caught instanceof ApiError && caught.status === 401) {
          navigate("/login");
          return;
        }
        setListError(tokenListError(caught));
      } finally {
        if (!cancelled) setLoading(false);
      }
    }
    void load();
    return () => {
      cancelled = true;
      controller.abort();
      actionRequest.current?.abort();
    };
  }, [
    identity?.installationId,
    identity?.nodeId,
    identity?.role,
    identity?.leadershipGeneration,
    identity?.latestKnownGeneration,
    identity?.stalePrimary,
    identity?.writable,
    clusterKeyID,
    navigate,
  ]);

  if (!canManageEnrollmentTokens(identity)) return null;

  async function createToken() {
    if (creating) return;
    const controller = beginActionRequest();
    setCreating(true);
    setFeedback("");
    setError("");
    try {
      const created = sanitizeTokenCreation(await createEnrollmentToken(controller.signal));
      if (controller.signal.aborted) return;
      setCreation(created);
      setTokens((current) => [creationSummary(created), ...current]);
    } catch (caught) {
      if (controller.signal.aborted) return;
      if (caught instanceof ApiError && caught.status === 401) {
        navigate("/login");
        return;
      }
      setError(tokenCreateError(caught));
    } finally {
      if (actionRequest.current === controller) actionRequest.current = undefined;
      if (!controller.signal.aborted) setCreating(false);
    }
  }

  async function copyToken() {
    const surface = tokenValueRef.current;
    if (!creation || !surface) return;
    setFeedback("");
    setError("");
    try {
      await copyTokenFromSurface(surface);
      setFeedback("Token copied");
    } catch {
      setError("Could not copy the token. Copy it manually from the surface above.");
    }
  }

  function downloadToken() {
    if (!creation) return;
    setFeedback("");
    setError("");
    downloadText(creation.token, TOKEN_FILENAME);
    setFeedback("Token download prepared");
  }

  function acknowledgeToken() {
    setCreation(undefined);
    setFeedback("Token saved. It will not be shown again.");
    setError("");
  }

  function beginRevoke(id: string) {
    setRevokeID(id);
    setRevokeConfirmation("");
    setFeedback("");
    setError("");
  }

  function cancelRevoke() {
    if (revoking) return;
    setRevokeID(undefined);
    setRevokeConfirmation("");
  }

  async function revoke(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!revokeID || revoking) return;
    const id = revokeID;
    const controller = beginActionRequest();
    setRevoking(true);
    setFeedback("");
    setError("");
    try {
      await revokeEnrollmentToken(id, revokeConfirmation as "replace-and-revoke", controller.signal);
      if (controller.signal.aborted) return;
      setTokens((current) => current.filter((token) => token.id !== id));
      setRevokeID(undefined);
      setRevokeConfirmation("");
      setFeedback("Token revoked");
    } catch (caught) {
      if (controller.signal.aborted) return;
      if (caught instanceof ApiError && caught.status === 401) {
        navigate("/login");
        return;
      }
      if (caught instanceof EnrollmentApiError && caught.status === 409 && revokeConfirmation !== REVOKE_CONFIRMATION) {
        setError(`Type ${REVOKE_CONFIRMATION} to confirm revocation.`);
      } else {
        setError(tokenRevokeError(caught));
      }
    } finally {
      if (actionRequest.current === controller) actionRequest.current = undefined;
      if (!controller.signal.aborted) setRevoking(false);
    }
  }

  function beginActionRequest() {
    actionRequest.current?.abort();
    const controller = new AbortController();
    actionRequest.current = controller;
    return controller;
  }

  return (
    <section className="mt-8 space-y-5 border-t border-line/80 pt-6" aria-labelledby="enrollment-token-title">
      <header className="border-b border-line/80 pb-5">
        <p className="pc-eyebrow pc-eyebrow-signal">owner authority</p>
        <h2 id="enrollment-token-title" className="pc-title mt-2 text-2xl text-mist">Enrollment tokens</h2>
        <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
          Create a one-time token for a NODE operator. The plaintext is shown only during acknowledgement and is never listed again.
        </p>
      </header>
      <div className="flex flex-wrap items-center justify-between gap-3 border-y border-line/80 px-1 py-4">
        <div><p className="pc-eyebrow">token authority</p><p className="mt-2 text-sm text-mute">Owner session required.</p></div>
        <button type="button" className="pc-btn" data-testid="create-enrollment-token" disabled={creating} onClick={() => void createToken()}>
          {creating ? "Creating…" : "Create enrollment token"}
        </button>
      </div>
      {creation ? (
        <section className="space-y-4 border-2 border-signal/60 bg-signal/5 p-4" aria-labelledby="new-enrollment-token-title" data-testid="enrollment-token-acknowledgement">
          <div><p className="pc-eyebrow pc-eyebrow-signal">save now</p><h3 id="new-enrollment-token-title" className="pc-title mt-2 text-xl text-mist">New enrollment token</h3></div>
          <textarea ref={tokenValueRef} className="pc-input min-h-24 resize-none font-mono text-sm" data-testid="enrollment-token-value" value={creation.token} readOnly aria-label="New enrollment token" />
          <p className="text-sm leading-6 text-mute">This token will not be shown again. Save it in a secure operator channel.</p>
          <div className="flex flex-wrap gap-2">
            <button type="button" className="pc-btn" data-testid="copy-enrollment-token" onClick={() => void copyToken()}>Copy token</button>
            <button type="button" className="pc-btn-ghost" data-testid="download-enrollment-token" onClick={downloadToken}>Download token</button>
            <button type="button" className="pc-btn-ghost" data-testid="acknowledge-enrollment-token" onClick={acknowledgeToken}>I have saved this token</button>
          </div>
        </section>
      ) : null}
      {feedback ? <p className="pc-toast-ok" role="status" aria-live="polite">{feedback}</p> : null}
      {error ? <p className="pc-toast-err" role="alert">{error}</p> : null}
      {loading ? (
        <p className="border-y border-dashed border-line py-5 text-sm text-faint" role="status">Loading enrollment tokens…</p>
      ) : listError ? (
        <p className="pc-toast-err" role="alert">{listError}</p>
      ) : tokens.length === 0 ? (
        <p className="border-y border-dashed border-line px-1 py-5 text-sm text-faint">No enrollment tokens have been created.</p>
      ) : (
        <div className="space-y-3" data-testid="enrollment-token-list">
          {tokens.map((token) => <TokenRow key={token.id} token={token} revokeOpen={revokeID === token.id} confirmation={revokeID === token.id ? revokeConfirmation : ""} revoking={revoking} onRevoke={() => beginRevoke(token.id)} onCancel={cancelRevoke} onConfirmationChange={setRevokeConfirmation} onSubmit={revoke} />)}
        </div>
      )}
    </section>
  );
}

function TokenRow({
  token, revokeOpen, confirmation, revoking, onRevoke, onCancel, onConfirmationChange, onSubmit,
}: {
  token: EnrollmentTokenSummary;
  revokeOpen: boolean;
  confirmation: string;
  revoking: boolean;
  onRevoke: () => void;
  onCancel: () => void;
  onConfirmationChange: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  const fields = [
    ["selector", token.selector], ["created", formatTokenDate(token.createdAt)],
    ["expires", formatTokenDate(token.expiresAt)], ["consumed", formatTokenDate(token.consumedAt)],
    ["revoked", formatTokenDate(token.revokedAt)],
  ];
  return (
    <article className="border-y border-line/80 px-1 py-4" data-testid={`enrollment-token-row-${token.id}`}>
      <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_auto] md:items-start">
        <dl className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">{fields.map(([label, value]) => <TokenField key={label} label={label} value={value} />)}</dl>
        {!revokeOpen ? <button type="button" className="pc-btn-ghost !text-xs" data-testid={`revoke-enrollment-token-${token.id}`} onClick={onRevoke}>Revoke</button> : null}
      </div>
      {revokeOpen ? (
        <form className="mt-4 flex flex-wrap items-end gap-2 border-t border-line/80 pt-4" data-testid={`revoke-enrollment-form-${token.id}`} onSubmit={onSubmit}>
          <label className="pc-label min-w-64 flex-1" htmlFor={`revoke-confirmation-${token.id}`}>Type {REVOKE_CONFIRMATION}
            <input id={`revoke-confirmation-${token.id}`} className="pc-input mt-2 font-mono text-sm" data-testid={`revoke-confirmation-${token.id}`} value={confirmation} onInput={(event) => onConfirmationChange(event.currentTarget.value)} onChange={(event) => onConfirmationChange(event.currentTarget.value)} disabled={revoking} autoComplete="off" />
          </label>
          <button type="submit" className="pc-btn" data-testid={`confirm-revoke-${token.id}`} disabled={revoking}>{revoking ? "Revoking…" : "Confirm revoke"}</button>
          <button type="button" className="pc-btn-ghost" data-testid={`cancel-revoke-${token.id}`} disabled={revoking} onClick={onCancel}>Cancel</button>
        </form>
      ) : null}
    </article>
  );
}

function TokenField({ label, value }: { label: string; value: string }) {
  return <div><dt className="pc-eyebrow">{label}</dt><dd className="mt-1 break-all font-mono text-xs text-mist">{value}</dd></div>;
}

function sanitizeTokenCreation(creation: EnrollmentTokenCreation): EnrollmentTokenCreation {
  return { id: creation.id, selector: creation.selector, token: creation.token, expiresAt: creation.expiresAt };
}

function sanitizeTokenSummary(token: EnrollmentTokenSummary): EnrollmentTokenSummary {
  return {
    id: token.id, selector: token.selector, createdAt: token.createdAt, expiresAt: token.expiresAt,
    consumedAt: token.consumedAt ?? null, revokedAt: token.revokedAt ?? null,
    ...(token.attemptSummary ? { attemptSummary: { count: token.attemptSummary.count, lastAttemptAt: token.attemptSummary.lastAttemptAt ?? null } } : {}),
  };
}

function creationSummary(creation: EnrollmentTokenCreation): EnrollmentTokenSummary {
  return { id: creation.id, selector: creation.selector, createdAt: new Date().toISOString(), expiresAt: creation.expiresAt, consumedAt: null, revokedAt: null };
}

function formatTokenDate(value?: string | null) {
  if (!value) return "—";
  const timestamp = Date.parse(value);
  return Number.isNaN(timestamp) ? "—" : new Date(timestamp).toISOString();
}

async function copyTokenFromSurface(surface: HTMLTextAreaElement) {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(surface.value); return; } catch { /* use the dedicated surface */ }
  }
  const previousFocus = document.activeElement;
  try {
    surface.focus(); surface.select();
    if (!(typeof document.execCommand === "function" && document.execCommand("copy"))) throw new Error("clipboard unavailable");
  } finally {
    if (previousFocus instanceof HTMLElement) previousFocus.focus();
  }
}

function downloadText(value: string, filename: string) {
  const url = URL.createObjectURL(new Blob([value], { type: "text/plain" }));
  try {
    const anchor = document.createElement("a");
    anchor.href = url; anchor.download = filename; anchor.setAttribute("data-testid", "enrollment-token-download-link");
    document.body.appendChild(anchor);
    try { anchor.click(); } finally { anchor.remove(); }
  } finally { URL.revokeObjectURL(url); }
}

function tokenListError(caught: unknown) {
  if (caught instanceof ApiError && caught.status === 403) return "Owner access is required to view enrollment tokens.";
  if (caught instanceof EnrollmentApiError && caught.kind === "server") return "Enrollment tokens are temporarily unavailable. Try again.";
  return "Enrollment tokens could not be loaded. Try again.";
}

function tokenCreateError(caught: unknown) {
  if (caught instanceof ApiError && caught.status === 403) return "Owner access is required to create an enrollment token.";
  if (caught instanceof EnrollmentApiError && caught.status === 409) return "The enrollment token could not be created.";
  return "The enrollment token could not be created. Try again.";
}

function tokenRevokeError(caught: unknown) {
  if (caught instanceof ApiError && caught.status === 403) return "Owner access is required to revoke enrollment tokens.";
  if (caught instanceof EnrollmentApiError && caught.status === 409) return "The enrollment token could not be revoked.";
  return "The enrollment token could not be revoked. Try again.";
}
