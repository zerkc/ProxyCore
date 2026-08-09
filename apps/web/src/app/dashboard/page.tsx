"use client";

import { useDashboard } from "./dashboard-context";
import { Overview } from "./views";

export default function DashboardOverviewPage() {
  const { status } = useDashboard();
  return <Overview status={status} />;
}
