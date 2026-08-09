export const dashboardNav = [
  {
    href: "/dashboard",
    id: "overview",
    label: "Pulse",
    title: "Control room",
  },
  {
    href: "/dashboard/dns",
    id: "dns",
    label: "DNS & proxy",
    title: "Zones",
  },
  {
    href: "/dashboard/certificates",
    id: "certificates",
    label: "Certificates",
    title: "Certificates",
  },
  {
    href: "/dashboard/ingress",
    id: "ingress",
    label: "Ingress",
    title: "Ingress",
  },
  {
    href: "/dashboard/streams",
    id: "streams",
    label: "Streams",
    title: "Streams",
  },
  {
    href: "/dashboard/operators",
    id: "operators",
    label: "Operators",
    title: "Operators",
  },
] as const;

export type DashboardNavId = (typeof dashboardNav)[number]["id"];

export function resolveDashboardNav(pathname: string) {
  const normalized =
    pathname.length > 1 && pathname.endsWith("/")
      ? pathname.slice(0, -1)
      : pathname;
  const exact = dashboardNav.find((item) => item.href === normalized);
  if (exact) return exact;
  return (
    dashboardNav.find(
      (item) =>
        item.href !== "/dashboard" && normalized.startsWith(`${item.href}/`),
    ) ?? dashboardNav[0]
  );
}
