import { useEffect, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import {
  ApiError,
  getEnrollmentHostnames,
  updateEnrollmentHostnames,
} from "../api";
import type { EnrollmentHostnameConfig, TopologyIdentity } from "./types";

export function canEditEnrollmentHostnames(identity?: TopologyIdentity) {
  return (
    identity?.writable === true &&
    identity.role !== "node" &&
    identity.role !== "stale-primary"
  );
}

export function ContinuityView({ identity }: { identity?: TopologyIdentity }) {
  const navigate = useNavigate();
  const [config, setConfig] = useState<EnrollmentHostnameConfig>();
  const [hostnames, setHostnames] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [accessDenied, setAccessDenied] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const editable = canEditEnrollmentHostnames(identity) && !accessDenied;

  useEffect(() => {
    let cancelled = false;

    async function load() {
      try {
        const loaded = await getEnrollmentHostnames();
        if (cancelled) return;
        setConfig(loaded);
        setHostnames(enrollmentHostnameText(loaded));
        setAccessDenied(false);
        setError("");
      } catch (caught) {
        if (cancelled) return;
        if (caught instanceof ApiError && caught.status === 401) {
          navigate("/login");
          return;
        }
        if (caught instanceof ApiError && caught.status === 403) {
          setAccessDenied(true);
          setError("");
          return;
        }
        setError(apiErrorMessage(caught, "Enrollment hostnames could not be loaded"));
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    void load();
    return () => {
      cancelled = true;
    };
  }, [navigate]);

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!config || !editable) return;
    setSaving(true);
    setSaved(false);
    setError("");
    try {
      const canonical = await updateEnrollmentHostnames(
        hostnames.split(/\r?\n/).filter((entry) => entry.length > 0),
      );
      setConfig(canonical);
      setHostnames(enrollmentHostnameText(canonical));
      setSaved(true);
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 401) {
        navigate("/login");
        return;
      }
      setError(apiErrorMessage(caught, "Enrollment hostnames could not be saved"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="mt-8 space-y-6">
      <header className="border-b border-line/80 pb-5">
        <p className="pc-eyebrow pc-eyebrow-signal">continuity / enrollment</p>
        <h2 className="pc-title mt-2 text-2xl text-mist">
          Enrollment hostnames
        </h2>
        <p className="mt-3 max-w-2xl text-sm leading-6 text-mute">
          Configure exact DNS names or IP addresses that resolve to and reach this
          PRIMARY. They become TLS SANs for enrollment on port 3443.
        </p>
      </header>

      {loading ? (
        <p className="border-y border-dashed border-line py-5 text-sm text-faint" role="status">
          Loading enrollment hostname configuration…
        </p>
      ) : accessDenied ? (
        <p className="pc-toast-err" role="alert">
          Owner access is required to view enrollment hostname configuration.
        </p>
      ) : config ? (
        <section className="space-y-5" aria-labelledby="enrollment-hostnames-title">
          <div className="flex flex-wrap items-start justify-between gap-4 border-y border-line/80 px-1 py-4">
            <div>
              <p className="pc-eyebrow">configuration status</p>
              <h3 id="enrollment-hostnames-title" className="pc-title mt-2 text-xl text-mist">
                {config.configured ? "Configured" : "Not configured"}
              </h3>
            </div>
            <span className="font-mono text-xs uppercase tracking-[0.1em] text-faint">
              {config.hostnames.length} {config.hostnames.length === 1 ? "entry" : "entries"}
            </span>
          </div>

          <p className="border-l-2 border-signal/70 pl-3 text-sm leading-6 text-mute">
            Saving names only records future enrollment SANs. It does not start TLS
            or create enrollment tokens.
          </p>

          <form className="space-y-5" onSubmit={(event) => void save(event)}>
            <label className="pc-label" htmlFor="enrollment-hostnames">
              DNS names or IP literals
              <textarea
                id="enrollment-hostnames"
                value={hostnames}
                onChange={(event) => {
                  setHostnames(event.target.value);
                  setSaved(false);
                }}
                className="pc-input min-h-36 resize-y font-mono"
                rows={6}
                disabled={!editable || saving}
                placeholder="primary.example.lan\n192.168.1.10"
              />
              <span className="mt-2 text-xs font-normal normal-case tracking-normal text-faint">
                One exact value per line; up to 32 entries. The server validates and
                canonicalizes the saved list.
              </span>
            </label>

            {!editable ? (
              <p className="text-sm leading-6 text-mute" role="note">
                {readOnlyReason(identity)}
              </p>
            ) : null}
            {error ? (
              <p className="pc-toast-err" role="alert">
                {error}
              </p>
            ) : null}
            {saved ? (
              <p className="pc-toast-ok" role="status">
                Saved. The canonical server response is shown above.
              </p>
            ) : null}

            <div className="flex justify-end">
              <button className="pc-btn" type="submit" disabled={!editable || saving}>
                {saving ? "Saving…" : "Save hostnames"}
              </button>
            </div>
          </form>
        </section>
      ) : (
        <p className="pc-toast-err" role="alert">
          {error || "Enrollment hostnames could not be loaded"}
        </p>
      )}
    </div>
  );
}

function enrollmentHostnameText(config: EnrollmentHostnameConfig) {
  return config.hostnames.join("\n");
}

function readOnlyReason(identity?: TopologyIdentity) {
  if (!identity) return "Waiting for topology identity before enabling changes.";
  if (identity.role === "node") {
    return "NODE installations can read this configuration but cannot change it.";
  }
  if (identity.role === "stale-primary" || identity.stalePrimary) {
    return "A stale PRIMARY can read this configuration but cannot change it.";
  }
  return "Configuration writes are disabled for this topology identity.";
}

function apiErrorMessage(caught: unknown, fallback: string) {
  if (caught instanceof ApiError && caught.status === 403) {
    return "The server rejected this configuration write.";
  }
  return caught instanceof Error && caught.message ? caught.message : fallback;
}
