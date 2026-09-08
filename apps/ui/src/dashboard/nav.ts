export const dashboardNav = [
  {
    href: "/dashboard",
    id: "overview",
    label: "overview",
    title: "control room",
    icon: "overview",
  },
  {
    href: "/dashboard/dns",
    id: "dns",
    label: "dns",
    title: "zones",
    icon: "dns",
  },
  {
    href: "/dashboard/certificates",
    id: "certificates",
    label: "certificates",
    title: "certificates",
    icon: "certificates",
  },
  {
    href: "/dashboard/ingress",
    id: "ingress",
    label: "ingress",
    title: "ingress",
    icon: "ingress",
  },
  {
    href: "/dashboard/streams",
    id: "streams",
    label: "streams",
    title: "streams",
    icon: "streams",
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
