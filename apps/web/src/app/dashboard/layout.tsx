import type { ReactNode } from "react";
import { DashboardProvider } from "./dashboard-context";
import DashboardShell from "./DashboardShell";

export default function DashboardLayout({ children }: { children: ReactNode }) {
  return (
    <DashboardProvider>
      <DashboardShell>{children}</DashboardShell>
    </DashboardProvider>
  );
}
