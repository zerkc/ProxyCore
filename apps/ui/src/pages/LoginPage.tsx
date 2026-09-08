import { FormEvent, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { SESSION_USERNAME_KEY } from "../api";
import { PasswordInput } from "../components/PasswordInput";

export function LoginPage() {
  const navigate = useNavigate();
  const [username, setUsername] = useState("owner");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    const response = await fetch("/api/auth/login", {
      method: "POST",
      credentials: "include",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    const body = (await response.json()) as {
      error?: string;
      user?: { username?: string };
    };
    if (!response.ok) {
      setError(body.error ?? "Sign-in failed");
      return;
    }
    if (body.user?.username) {
      sessionStorage.setItem(SESSION_USERNAME_KEY, body.user.username);
    }
    navigate("/dashboard");
  }

  return (
    <main className="min-h-screen bg-bay p-3 sm:p-6">
      <div className="mx-auto flex min-h-[calc(100vh-1.5rem)] max-w-[1280px] flex-col border border-line/80 bg-panel sm:min-h-[calc(100vh-3rem)]">
        <header className="flex items-center justify-between gap-4 border-b border-line/80 px-4 py-4 md:px-6">
          <div className="flex min-w-0 items-baseline gap-2">
            <span className="pc-title text-lg lowercase text-mist">proxycore</span>
            <span className="truncate font-mono text-xs text-mute">
              / local control plane
            </span>
          </div>
          <span className="shrink-0 font-mono text-[11px] uppercase tracking-[0.16em] text-faint">
            locked
          </span>
        </header>

        <section className="flex flex-1 items-start justify-center bg-bay px-4 py-10 md:px-8 md:py-16">
          <div className="w-full max-w-xl">
            <p className="font-mono text-xs text-signal">
              operator@proxycore:~$ authenticate
            </p>
            <form
              onSubmit={submit}
              className="pc-enter mt-3 w-full border-y border-line/80 bg-panel/40 p-5 font-mono sm:p-8"
            >
              <p className="pc-eyebrow">session / login</p>
              <h1 className="mt-4 font-mono text-xl font-medium lowercase tracking-tight text-mist/90">
                return to the control room
              </h1>
              <p className="mt-3 text-sm leading-6 text-mute">
                Local credentials only. Sessions can be revoked by an Owner.
              </p>
              <div className="mt-8 space-y-5">
                <label className="pc-label">
                  Username
                  <input
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                    className="pc-input"
                    autoComplete="username"
                    required
                  />
                </label>
                <PasswordInput
                  label="Password"
                  value={password}
                  onChange={setPassword}
                  autoComplete="current-password"
                  required
                  className="pc-input"
                />
              </div>
              {error ? (
                <p className="pc-toast-err !mt-5" role="alert">
                  {error}
                </p>
              ) : null}
              <button className="pc-btn mt-7 w-full" type="submit">
                Sign in
              </button>
              <Link
                className="mt-5 block text-center text-sm text-mute underline underline-offset-4 transition hover:text-mist"
                to="/bootstrap"
              >
                New installation? Bootstrap Owner
              </Link>
            </form>
          </div>
        </section>

        <footer className="flex flex-col gap-4 border-t border-line/80 bg-panel/70 px-4 py-4 md:flex-row md:items-end md:justify-between md:px-6">
          <dl className="grid gap-x-8 gap-y-2 font-mono text-[11px] sm:grid-cols-3">
            <div className="flex gap-3">
              <dt className="text-faint">mode</dt>
              <dd className="text-mute">single-host</dd>
            </div>
            <div className="flex gap-3">
              <dt className="text-faint">auth</dt>
              <dd className="text-mute">local credentials</dd>
            </div>
            <div className="flex gap-3">
              <dt className="text-faint">network</dt>
              <dd className="text-mute">private by default</dd>
            </div>
          </dl>
          <p className="shrink-0 font-mono text-xs text-signal">
            <span aria-hidden="true">●</span> awaiting operator session
          </p>
        </footer>
      </div>
    </main>
  );
}
