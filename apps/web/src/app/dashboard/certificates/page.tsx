"use client";

import { CertificatesView } from "../CertificatesView";
import { useDashboard } from "../dashboard-context";

export default function DashboardCertificatesPage() {
  const { status, refresh, setMessage, setError } = useDashboard();

  return (
    <CertificatesView
      certificates={status?.certificates ?? []}
      onRefresh={refresh}
      onMessage={setMessage}
      onError={setError}
    />
  );
}
