import { useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  BackupApiError,
  SESSION_USERNAME_KEY,
  exportBackup,
  importBackup,
  type BackupImportReport,
} from "../api";
import { PasswordInput } from "../components/PasswordInput";

type Busy = "export" | "preview" | "restore" | undefined;

export function BackupView() {
  const navigate = useNavigate();
  const [busy, setBusy] = useState<Busy>();
  const [exportPassphrase, setExportPassphrase] = useState("");
  const [exportFeedback, setExportFeedback] = useState("");
  const [exportError, setExportError] = useState("");
  const [file, setFile] = useState<File>();
  const [importPassphrase, setImportPassphrase] = useState("");
  const [report, setReport] = useState<BackupImportReport>();
  const [importError, setImportError] = useState("");

  async function runExport() {
    if (busy) return;
    setBusy("export");
    setExportFeedback("");
    setExportError("");
    try {
      const result = await exportBackup(exportPassphrase || undefined);
      downloadBlob(result.blob, result.filename);
      setExportFeedback(`Backup exported as ${result.filename}.`);
    } catch (caught) {
      setExportError(backupErrorMessage(caught, "export"));
    } finally {
      setBusy(undefined);
    }
  }

  async function runImport(dryRun: boolean) {
    if (busy || !file) return;
    setBusy(dryRun ? "preview" : "restore");
    setImportError("");
    try {
      const result = await importBackup(file, {
        passphrase: importPassphrase || undefined,
        dryRun,
      });
      if (dryRun) {
        setReport(result);
        return;
      }
      // A real import deletes every session server-side; drop client state too.
      clearClientSession();
      setImportPassphrase("");
      navigate("/login");
    } catch (caught) {
      if (dryRun) setReport(undefined);
      setImportError(backupErrorMessage(caught, "import"));
    } finally {
      setBusy(undefined);
    }
  }

  function selectFile(selected: File | undefined) {
    setFile(selected);
    setReport(undefined);
    setImportError("");
  }

  function changeImportPassphrase(value: string) {
    setImportPassphrase(value);
    setReport(undefined);
  }

  return (
    <section className="space-y-6" aria-labelledby="backup-title">
      <header className="border-b border-line/80 pb-4 md:pb-5">
        <p className="pc-eyebrow">owner authority</p>
        <h1 id="backup-title" className="pc-title mt-2 text-3xl text-mist">
          backup
        </h1>
        <p className="mt-2 max-w-2xl text-sm leading-6 text-mute">
          Export the full configuration of this installation, or restore it from
          a previous bundle. Owner access and a configured master key are
          required.
        </p>
      </header>

      <section
        className="space-y-4 border-y border-line/80 px-1 py-5"
        aria-labelledby="backup-export-title"
        data-testid="backup-export"
      >
        <div>
          <p className="pc-eyebrow">export</p>
          <h2
            id="backup-export-title"
            className="pc-title mt-2 text-xl text-mist"
          >
            Export backup
          </h2>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-mute">
            Optionally protect secrets in the bundle with a passphrase. You will
            need the same passphrase to restore it.
          </p>
        </div>
        <div className="max-w-md">
          <PasswordInput
            id="backup-export-passphrase"
            label="Passphrase (optional)"
            value={exportPassphrase}
            onChange={setExportPassphrase}
            autoComplete="new-password"
            className="pc-input mt-2"
            disabled={busy !== undefined}
          />
        </div>
        <button
          type="button"
          className="pc-btn"
          data-testid="backup-export-submit"
          disabled={busy !== undefined}
          onClick={() => void runExport()}
        >
          {busy === "export" ? "Exporting…" : "Export backup"}
        </button>
        {exportFeedback ? (
          <p className="pc-toast-ok" role="status" aria-live="polite">
            {exportFeedback}
          </p>
        ) : null}
        {exportError ? (
          <p className="pc-toast-err" role="alert">
            {exportError}
          </p>
        ) : null}
      </section>

      <section
        className="space-y-4 border-b border-line/80 px-1 pb-5"
        aria-labelledby="backup-import-title"
        data-testid="backup-import"
      >
        <div>
          <p className="pc-eyebrow">restore</p>
          <h2
            id="backup-import-title"
            className="pc-title mt-2 text-xl text-mist"
          >
            Import backup
          </h2>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-mute">
            Choose a backup bundle and preview what it would change before
            restoring.
          </p>
        </div>
        <div className="grid max-w-md gap-4">
          <label className="pc-label" htmlFor="backup-import-file">
            Backup file (.zip)
            <input
              id="backup-import-file"
              type="file"
              accept=".zip,application/zip"
              className="pc-input mt-2"
              disabled={busy !== undefined}
              onChange={(event) => selectFile(event.target.files?.[0])}
            />
          </label>
          <PasswordInput
            id="backup-import-passphrase"
            label="Passphrase (if the backup was exported with one)"
            value={importPassphrase}
            onChange={changeImportPassphrase}
            autoComplete="off"
            className="pc-input mt-2"
            disabled={busy !== undefined}
          />
        </div>
        <button
          type="button"
          className="pc-btn-ghost"
          data-testid="backup-preview-submit"
          disabled={busy !== undefined || !file}
          onClick={() => void runImport(true)}
        >
          {busy === "preview" ? "Previewing…" : "Preview import"}
        </button>
        {importError ? (
          <p className="pc-toast-err" role="alert">
            {importError}
          </p>
        ) : null}
        {report ? (
          <ImportPreview
            report={report}
            restoring={busy === "restore"}
            disabled={busy !== undefined}
            onRestore={() => void runImport(false)}
          />
        ) : null}
      </section>
    </section>
  );
}

function ImportPreview({
  report,
  restoring,
  disabled,
  onRestore,
}: {
  report: BackupImportReport;
  restoring: boolean;
  disabled: boolean;
  onRestore: () => void;
}) {
  const certs = report.certsToRestore?.length ?? 0;
  const conflicts = report.compatibilityConflicts ?? [];
  const metadata: Array<[string, string]> = [
    ["created", report.createdAt],
    ["exporter version", report.exporterVersion],
    ["format version", String(report.formatVersion)],
    ["node ID", report.nodeId],
  ];
  return (
    <div className="space-y-4" data-testid="backup-preview">
      <p className="pc-eyebrow pc-eyebrow-signal">dry-run preview</p>
      <dl className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
        {metadata.map(([label, value]) => (
          <div key={label}>
            <dt className="pc-eyebrow">{label}</dt>
            <dd className="mt-1 break-all font-mono text-xs text-mist">
              {value}
            </dd>
          </div>
        ))}
      </dl>
      <div className="overflow-x-auto">
        <table
          className="w-full text-left text-sm"
          data-testid="backup-preview-tables"
        >
          <thead>
            <tr className="pc-eyebrow">
              <th className="py-2 pr-4">table</th>
              <th className="py-2 pr-4">rows</th>
              <th className="py-2">action</th>
            </tr>
          </thead>
          <tbody>
            {report.tables.map((table) => (
              <tr key={table.name} className="border-t border-line/70">
                <td className="py-2 pr-4 font-mono text-xs text-mist">
                  {table.name}
                </td>
                <td className="py-2 pr-4 font-mono text-xs">
                  {table.rowCount}
                </td>
                <td className="py-2 font-mono text-xs">{table.action}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ul className="space-y-1 text-sm text-mute">
        <li>Environment changes: {report.envWouldChange ? "yes" : "no"}</li>
        <li>
          Certificates to restore: {certs}{" "}
          {certs === 1 ? "certificate" : "certificates"}
        </li>
      </ul>
      {conflicts.length > 0 ? (
        <div className="pc-toast-err" data-testid="backup-conflicts">
          <p>Compatibility conflicts:</p>
          <ul className="mt-1 list-disc pl-5">
            {conflicts.map((conflict) => (
              <li key={conflict}>{conflict}</li>
            ))}
          </ul>
        </div>
      ) : null}
      <div className="space-y-3 border-2 border-danger/50 p-4">
        <p className="text-sm leading-6 text-mist">
          Restoring replaces all configuration on this installation with the
          contents of this backup and signs everyone out, including you.
        </p>
        <button
          type="button"
          className="pc-btn"
          data-testid="backup-restore-submit"
          disabled={disabled}
          onClick={onRestore}
        >
          {restoring ? "Restoring…" : "Restore backup"}
        </button>
      </div>
    </div>
  );
}

function backupErrorMessage(caught: unknown, operation: "export" | "import") {
  if (caught instanceof BackupApiError) {
    switch (caught.kind) {
      case "master_key_mismatch":
        return "This backup was created with a different master key. Configure the original master key on this installation and try again.";
      case "passphrase_mismatch":
        return "The passphrase is incorrect for this backup.";
      case "passphrase_required":
        return "This backup is encrypted. Enter its passphrase.";
      case "invalid_bundle":
        return "The file is not a valid ProxyCore backup (it is corrupt or from an unsupported version).";
      case "too_large":
        return "The backup file is too large (the limit is 4 GiB).";
      case "unavailable":
        return "Backup is unavailable because the master key is not configured on this installation.";
      case "unauthorized":
      case "forbidden":
        return `Owner access is required to ${operation} a backup.`;
    }
  }
  return `The backup ${operation} failed. Try again.`;
}

function clearClientSession() {
  try {
    sessionStorage.removeItem(SESSION_USERNAME_KEY);
  } catch {
    // Storage may be unavailable; the server session is already gone.
  }
}

// Browsers start the download asynchronously after click(); revoking the blob
// URL right away can leave an empty file, so the revoke is deferred.
const BLOB_URL_REVOKE_DELAY_MS = 30_000;

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  try {
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = filename;
    document.body.appendChild(anchor);
    try {
      anchor.click();
    } finally {
      anchor.remove();
    }
  } catch (error) {
    URL.revokeObjectURL(url);
    throw error;
  }
  setTimeout(() => URL.revokeObjectURL(url), BLOB_URL_REVOKE_DELAY_MS);
}
