"use client";

import { FormEvent, useState } from "react";
import { useRouter } from "next/navigation";

export default function LoginPage() {
  const router = useRouter();
  const [username, setUsername] = useState("owner");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    const response = await fetch("/api/auth/login", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!response.ok) {
      setError((await response.json()).error ?? "Sign-in failed");
      return;
    }
    router.push("/dashboard");
  }

  return (
    <main className="grid min-h-screen place-items-center px-6 py-12">
      <form
        onSubmit={submit}
        className="pc-panel pc-enter w-full max-w-md p-8 shadow-2xl shadow-black/30"
      >
        <p className="pc-title text-3xl text-mist">ProxyCore</p>
        <h1 className="mt-3 text-xl font-medium tracking-tight text-mist/90">
          Return to the control room
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
          <label className="pc-label">
            Password
            <input
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              className="pc-input"
              type="password"
              autoComplete="current-password"
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
          Sign in
        </button>
        <a
          className="mt-5 block text-center text-sm text-mute underline underline-offset-4 transition hover:text-mist"
          href="/bootstrap"
        >
          New installation? Bootstrap Owner
        </a>
      </form>
    </main>
  );
}
