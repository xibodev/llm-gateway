import {
  Activity,
  BellRing,
  BarChart3,
  Boxes,
  KeyRound,
  LayoutDashboard,
  Play,
  Plug,
  Route,
  ScrollText,
  Settings,
  Users,
} from "lucide-preact";

export type PageID =
  | "overview"
  | "providers"
  | "routes"
  | "models"
  | "playground"
  | "keys"
  | "access"
  | "usage"
  | "requests"
  | "alerts"
  | "audit"
  | "settings";

export type NavigationItem = {
  id: PageID;
  label: string;
  description: string;
  Icon: typeof LayoutDashboard;
};

const adminNavigation: NavigationItem[] = [
  { id: "overview", label: "Overview", description: "Workspace health", Icon: LayoutDashboard },
  { id: "providers", label: "Providers", description: "Connections and catalogs", Icon: Plug },
  { id: "routes", label: "Routes", description: "Ordered failover", Icon: Route },
  { id: "models", label: "Models & endpoints", description: "Catalog and setup", Icon: Boxes },
  { id: "playground", label: "Playground", description: "Run gateway requests", Icon: Play },
  { id: "keys", label: "API keys", description: "Lifecycle and policy", Icon: KeyRound },
  { id: "access", label: "Access", description: "Principals, projects, memberships", Icon: Users },
  { id: "usage", label: "Usage & quotas", description: "Consumption and advisory status", Icon: BarChart3 },
  { id: "requests", label: "Requests", description: "Recorded requests and failover", Icon: Activity },
  { id: "alerts", label: "Alerts", description: "Quota and expiry notifications", Icon: BellRing },
  { id: "audit", label: "Audit log", description: "Administrative activity", Icon: ScrollText },
  { id: "settings", label: "Settings", description: "Gateway-wide options", Icon: Settings },
];

const portalNavigation: NavigationItem[] = adminNavigation.filter((item) =>
  ["overview", "providers", "models", "playground", "keys", "usage", "requests", "settings"].includes(item.id),
).map((item) => item.id === "requests" ? { ...item, description: "Your recorded requests" } : item);

export function navigationFor(mode: "admin" | "portal"): NavigationItem[] {
  return mode === "portal" ? portalNavigation : adminNavigation;
}
