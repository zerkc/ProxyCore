import { useEffect } from "react";
import { Navigate, Route, Routes, useParams } from "react-router-dom";
import { CertificatesView } from "./dashboard/CertificatesView";
import { DashboardProvider, useDashboard } from "./dashboard/dashboard-context";
import DashboardShell from "./dashboard/DashboardShell";
import { DnsZonesView } from "./dashboard/dns/DnsZonesView";
import { ZoneRecordsView } from "./dashboard/dns/ZoneRecordsView";
import { IngressView } from "./dashboard/IngressView";
import { StreamsView } from "./dashboard/streams/StreamsView";
import { Overview } from "./dashboard/views";
import { BootstrapPage } from "./pages/BootstrapPage";
import { LoginPage } from "./pages/LoginPage";

export function App() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/login" replace />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/bootstrap" element={<BootstrapPage />} />
      <Route path="/dashboard/*" element={<DashboardRoutes />} />
      <Route path="*" element={<Navigate to="/login" replace />} />
    </Routes>
  );
}

function DashboardRoutes() {
  return (
    <DashboardProvider>
      <DashboardShell>
        <Routes>
          <Route index element={<DashboardOverviewPage />} />
          <Route path="dns" element={<DashboardDnsPage />} />
          <Route path="dns/:zoneId" element={<DashboardZonePage />} />
          <Route
            path="certificates"
            element={<DashboardCertificatesPage />}
          />
          <Route path="ingress" element={<IngressView />} />
          <Route path="streams" element={<DashboardStreamsPage />} />
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </DashboardShell>
    </DashboardProvider>
  );
}

function DashboardOverviewPage() {
  const { status } = useDashboard();
  return <Overview status={status} />;
}

function DashboardDnsPage() {
  const { status, openZoneDialog } = useDashboard();

  return (
    <DnsZonesView
      status={status}
      openCreate={openZoneDialog}
    />
  );
}

function DashboardZonePage() {
  const { zoneId } = useParams();
  const { status, setSelectedZone, openCreateRecord, openEditRecord } =
    useDashboard();

  useEffect(() => {
    if (zoneId) {
      setSelectedZone(zoneId);
    }
  }, [zoneId, setSelectedZone]);

  const zone = status?.zones.find((item) => item.id === zoneId);

  return (
    <ZoneRecordsView
      zone={zone}
      openCreate={openCreateRecord}
      openEdit={openEditRecord}
    />
  );
}

function DashboardCertificatesPage() {
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

function DashboardStreamsPage() {
  const { status, saveStream, updateStream, deleteStream } = useDashboard();

  return (
    <StreamsView
      streams={status?.streams ?? []}
      saveStream={saveStream}
      updateStream={updateStream}
      deleteStream={deleteStream}
    />
  );
}
