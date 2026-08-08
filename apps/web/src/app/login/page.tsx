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
        className="w-full max-w-md rounded-[2rem] border border-slate-700 bg-slate-950/80 p-8 shadow-2xl shadow-slate-950/40"
      >
        <p className="text-xs uppercase tracking-[0.32em] text-emerald-300">ProxyCore</p>
        <h1 className="mt-4 text-3xl font-semibold tracking-tight">Return to the control room</h1>
        <p className="mt-3 text-sm leading-6 text-slate-400">
          Local credentials only. Sessions can be revoked by an Owner.
        </p>
        <div className="mt-8 space-y-5">
          <label className="block text-sm text-slate-300">
            Username
            <input
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-900 px-4 py-3 outline-none transition focus:border-emerald-300"
              autoComplete="username"
              required
            />
          </label>
          <label className="block text-sm text-slate-300">
            Password
            <input
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              className="mt-2 w-full rounded-xl border border-slate-700 bg-slate-900 px-4 py-3 outline-none transition focus:border-emerald-300"
              type="password"
              autoComplete="current-password"
              required
            />
          </label>
        </div>
        {error ? (
          <p className="mt-5 rounded-xl border border-red-400/30 bg-red-400/10 p-3 text-sm text-red-200" role="alert">
            {error}
          </p>
        ) : null}
        <button
          className="mt-7 w-full rounded-xl bg-emerald-300 px-4 py-3 font-semibold text-slate-950 transition hover:bg-emerald-200 focus:outline-none focus:ring-2 focus:ring-emerald-200 focus:ring-offset-2 focus:ring-offset-slate-950"
          type="submit"
        >
          Sign in
        </button>
        <a className="mt-5 block text-center text-sm text-slate-400 underline underline-offset-4" href="/bootstrap">
          New installation? Bootstrap Owner
        </a>
      </form>
    </main>
  );
}
