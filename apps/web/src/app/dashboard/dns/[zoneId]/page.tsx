"use client";

import { use, useEffect } from "react";
import { useDashboard } from "../../dashboard-context";
import { ZoneRecordsView } from "../ZoneRecordsView";

export default function DashboardZonePage({
  params,
}: {
  params: Promise<{ zoneId: string }>;
}) {
  const { zoneId } = use(params);
  const {
    status,
    setSelectedZone,
    openCreateRecord,
    openEditRecord,
  } = useDashboard();

  useEffect(() => {
    setSelectedZone(zoneId);
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
