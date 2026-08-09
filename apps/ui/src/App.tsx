import { useEffect, useState, type FormEvent } from "react";
import { Link, Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { api, type PublicUser } from "./api";

type Health = {
  ok: boolean;
  service?: string;
  timestamp?: string;
};

type StatusPayload = {
  desiredRevision?: { revisionNumber?: number; checksum?: string };
  appliedRevision?: { revisionNumber?: number; checksum?: string };
};

export function App() {
  return (
    <div className="shell">
      <header className="top">
        <Link to="/" className="brand">
          ProxyCore
        </Link>
        <nav>
          <Link to="/">Pulse</Link>
          <Link to="/login">Login</Link>
          <Link to="/bootstrap">Bootstrap</Link>
          <Link to="/dashboard">Dashboard</Link>
        </nav>
      </header>
      <main>
        <Routes>
          <Route path="/" element={<PulsePage />} />
          <Route path="/login" element={<LoginPage />} />
          <Route path="/bootstrap" element={<BootstrapPage />} />
          <Route path="/dashboard/*" element={<DashboardPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}

function PulsePage() {
  const [health, setHealth] = useState<Health | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api<Health>("/api/health")
      .then((body) => {
        if (!cancelled) setHealth(body);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <section className="panel">
      <p className="eyebrow">Control plane</p>
      <h1>Go API + Vite SPA</h1>
      <p className="lede">
        Next.js is off the default Compose path. The Go edge serves this UI and
        auth; configuration routes are proxied to a Node tsx API until they are
        ported.
      </p>
      <dl className="status">
        <div>
          <dt>API</dt>
          <dd>{error ? `error: ${error}` : health?.ok ? "ok" : "checking…"}</dd>
        </div>
        <div>
          <dt>Service</dt>
          <dd>{health?.service ?? "—"}</dd>
        </div>
        <div>
          <dt>UTC</dt>
          <dd>{health?.timestamp ?? "—"}</dd>
        </div>
      </dl>
    </section>
  );
}

function LoginPage() {
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    try {
      await api<{ user: PublicUser }>("/api/auth/login", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      navigate("/dashboard");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <section className="panel">
      <p className="eyebrow">Operator access</p>
      <h1>Login</h1>
      <form className="stack" onSubmit={onSubmit}>
        <label>
          Username
          <input
            className="pc-input"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            required
          />
        </label>
        <label>
          Password
          <input
            className="pc-input"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            minLength={5}
            required
          />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <button type="submit">Sign in</button>
      </form>
    </section>
  );
}

function BootstrapPage() {
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    try {
      await api<{ user: PublicUser }>("/api/auth/bootstrap", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      navigate("/login");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <section className="panel">
      <p className="eyebrow">First Owner</p>
      <h1>Bootstrap</h1>
      <p className="lede">Creates the only initial Owner. Later attempts fail.</p>
      <form className="stack" onSubmit={onSubmit}>
        <label>
          Username
          <input
            className="pc-input"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            required
          />
        </label>
        <label>
          Password
          <input
            className="pc-input"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={5}
            required
          />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <button type="submit">Create Owner</button>
      </form>
    </section>
  );
}

function DashboardPage() {
  const navigate = useNavigate();
  const [status, setStatus] = useState<StatusPayload | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api<StatusPayload>("/api/status")
      .then((body) => {
        if (!cancelled) setStatus(body);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          const message = err instanceof Error ? err.message : String(err);
          setError(message);
          if (message.toLowerCase().includes("auth")) {
            navigate("/login");
          }
        }
      });
    return () => {
      cancelled = true;
    };
  }, [navigate]);

  async function logout() {
    await api("/api/auth/logout", { method: "POST" });
    navigate("/login");
  }

  return (
    <section className="panel">
      <p className="eyebrow">Pulse</p>
      <h1>Dashboard</h1>
      <p className="lede">
        Full desk UI (DNS, certs, streams) moves here next; status is live via
        the proxied configuration API.
      </p>
      {error ? <p className="error">{error}</p> : null}
      <dl className="status">
        <div>
          <dt>Desired</dt>
          <dd>
            {status?.desiredRevision?.revisionNumber ?? "—"}{" "}
            {status?.desiredRevision?.checksum
              ? `(${status.desiredRevision.checksum.slice(0, 12)})`
              : ""}
          </dd>
        </div>
        <div>
          <dt>Applied</dt>
          <dd>
            {status?.appliedRevision?.revisionNumber ?? "—"}{" "}
            {status?.appliedRevision?.checksum
              ? `(${status.appliedRevision.checksum.slice(0, 12)})`
              : ""}
          </dd>
        </div>
      </dl>
      <button type="button" onClick={() => void logout()}>
        Log out
      </button>
    </section>
  );
}
