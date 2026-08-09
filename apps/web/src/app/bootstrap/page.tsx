"use client";

import { FormEvent, useState } from "react";
import { useRouter } from "next/navigation";

export default function BootstrapPage() {
  const router = useRouter();
  const [username, setUsername] = useState("owner");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    const response = await fetch("/api/auth/bootstrap", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!response.ok) {
      setError((await response.json()).error ?? "Bootstrap failed");
      return;
    }
    router.push("/login");
  }

  return (
    <main className="grid min-h-screen place-items-center px-6 py-12">
      <form
        onSubmit={submit}
        className="pc-panel pc-enter w-full max-w-md p-8 shadow-2xl shadow-black/30"
      >
        <p className="pc-eyebrow pc-eyebrow-signal">First light</p>
        <h1 className="pc-title mt-4 text-3xl text-mist">
          Claim this installation
        </h1>
        <p className="mt-3 text-sm leading-6 text-mute">
          Create the first Owner. This is the only moment when ProxyCore can be
          initialized without an existing session.
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
          <label className="pc-label">
            Password
            <input
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              className="pc-input"
              type="password"
              autoComplete="new-password"
              minLength={12}
              required
            />
          </label>
        </div>
        {error ? (
          <p className="pc-toast-err !mt-5" role="alert">
            {error}
          </p>
        ) : null}
        <button className="pc-btn mt-7 w-full" type="submit">
          Create Owner
        </button>
        <a
          className="mt-5 block text-center text-sm text-mute underline underline-offset-4 transition hover:text-mist"
          href="/login"
        >
          Already initialized? Sign in
        </a>
      </form>
    </main>
  );
}
