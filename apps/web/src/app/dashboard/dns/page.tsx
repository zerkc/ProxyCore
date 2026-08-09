"use client";

import { useDashboard } from "../dashboard-context";
import { DnsZonesView } from "./DnsZonesView";

export default function DashboardDnsPage() {
  const { status, zoneName, setZoneName, createZone } = useDashboard();

  return (
    <DnsZonesView
      status={status}
      zoneName={zoneName}
      setZoneName={setZoneName}
      createZone={createZone}
    />
  );
}
