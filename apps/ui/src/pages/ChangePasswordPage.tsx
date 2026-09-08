import { FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { PasswordInput } from "../components/PasswordInput";

export function ChangePasswordPage() {
  const navigate = useNavigate();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (password !== confirm) {
      setError("Passwords do not match");
      return;
    }
    const response = await fetch("/api/auth/change-password", {
      method: "POST",
      credentials: "include",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ password }),
    });
    const body = (await response.json().catch(() => ({}))) as {
      error?: string;
      code?: string;
    };
    if (!response.ok) {
      if (response.status === 401 || body.code === "AUTH_REQUIRED") {
        navigate("/login", { replace: true });
        return;
      }
      setError(
        body.code === "PASSWORD_INVALID"
          ? (body.error ?? "Choose a stronger password")
          : (body.error ?? "Password change failed"),
      );
      return;
    }
    navigate("/dashboard", { replace: true });
  }

  async function logout() {
    await fetch("/api/auth/logout", {
      method: "POST",
      credentials: "include",
    });
    navigate("/login", { replace: true });
  }

  return (
    <main className="min-h-screen bg-bay p-3 sm:p-6">
      <section className="mx-auto mt-10 w-full max-w-xl border-y border-line/80 bg-panel/40 p-5 font-mono sm:p-8">
        <p className="pc-eyebrow">session / password reset</p>
        <h1 className="mt-4 text-xl font-medium lowercase text-mist/90">
          change your temporary password
        </h1>
        <p className="mt-3 text-sm leading-6 text-mute">
          Access remains restricted until a new password is saved.
        </p>
        <form onSubmit={submit} className="mt-8 space-y-5">
          <PasswordInput
            label="New password"
            value={password}
            onChange={setPassword}
            autoComplete="new-password"
            required
            className="pc-input"
          />
          <PasswordInput
            label="Confirm new password"
            value={confirm}
            onChange={setConfirm}
            autoComplete="new-password"
            required
            className="pc-input"
          />
          {error ? (
            <p className="pc-toast-err" role="alert">
              {error}
            </p>
          ) : null}
          <button className="pc-btn w-full" type="submit">
            Save password
          </button>
          <button
            className="w-full text-sm text-mute underline underline-offset-4"
            type="button"
            onClick={logout}
          >
            Sign out
          </button>
        </form>
      </section>
    </main>
  );
}
