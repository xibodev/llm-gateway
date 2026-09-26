import { useEffect, useState } from "preact/hooks";
import { LogOut, Menu, Moon, Sun, X } from "lucide-preact";
import type { ComponentChildren } from "preact";
import type { ConsoleMode } from "../lib/mode";
import type { NavigationItem, PageID } from "../lib/navigation";

export function AppShell({
  mode,
  navigation,
  currentPage,
  onNavigate,
  identityName = "",
  identityDetail = "",
  staticAdminKeyActive = false,
  onStaticAdminSignOut,
  children,
}: {
  mode: ConsoleMode;
  navigation: NavigationItem[];
  currentPage: PageID;
  onNavigate: (page: PageID) => void;
  identityName?: string;
  identityDetail?: string;
  staticAdminKeyActive?: boolean;
  onStaticAdminSignOut?: () => void;
  children: ComponentChildren;
}) {
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [theme, setTheme] = useState<"light" | "dark">(() => {
    const saved = window.localStorage.getItem("llmgw.console.theme");
    if (saved === "light" || saved === "dark") return saved;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  });

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    window.localStorage.setItem("llmgw.console.theme", theme);
  }, [theme]);

  const selectPage = (page: PageID) => {
    onNavigate(page);
    setDrawerOpen(false);
  };

  return (
    <div class="console-shell">
      <button
        class={`drawer-scrim ${drawerOpen ? "drawer-scrim--visible" : ""}`}
        type="button"
        aria-label="Close navigation"
        tabIndex={drawerOpen ? 0 : -1}
        onClick={() => setDrawerOpen(false)}
      />
      <aside class={`sidebar ${drawerOpen ? "sidebar--open" : ""}`}>
        <div class="brand-row">
          <span class="brand-mark">LG</span>
          <span><strong>LLM Gateway</strong><small>{mode === "portal" ? "Owner portal" : "Operations"}</small></span>
          <button class="icon-button sidebar-close" type="button" aria-label="Close navigation" onClick={() => setDrawerOpen(false)}><X size={18} /></button>
        </div>
        <nav aria-label="Console navigation">
          <p class="nav-label">Workspace</p>
          {navigation.map((item) => (
            <button
              class={`nav-item ${item.id === currentPage ? "nav-item--active" : ""}`}
              type="button"
              key={item.id}
              aria-current={item.id === currentPage ? "page" : undefined}
              onClick={() => selectPage(item.id)}
            >
              <item.Icon size={18} strokeWidth={1.8} aria-hidden="true" />
              <span><strong>{item.label}</strong><small>{item.description}</small></span>
            </button>
          ))}
        </nav>
        <div class="sidebar-footer"><span class="status-dot" aria-hidden="true" /> Local control plane</div>
      </aside>
      <section class="shell-main">
        <header class="topbar">
          <button class="icon-button menu-button" type="button" aria-label="Open navigation" onClick={() => setDrawerOpen(true)}><Menu size={20} /></button>
          <div class="topbar__context"><span class="topbar__label">{mode === "portal" ? "Private human workspace" : "Gateway control plane"}</span><span class="topbar__mode">{mode === "portal" ? "Portal" : "Admin"}</span></div>
          {mode === "portal" && identityName ? <div class="topbar__identity" aria-label={`Signed in as ${identityName}`}><span class="topbar__avatar" aria-hidden="true">{identityName.slice(0, 1).toUpperCase()}</span><span><strong>{identityName}</strong>{identityDetail && identityDetail !== identityName ? <small>{identityDetail}</small> : null}</span></div> : null}
          {mode === "admin" && staticAdminKeyActive && onStaticAdminSignOut ? <button class="button button--secondary" type="button" onClick={onStaticAdminSignOut}><LogOut size={16} /> Sign out</button> : null}
          <button
            class="icon-button topbar__layout"
            type="button"
            aria-label={theme === "dark" ? "Use light theme" : "Use dark theme"}
            title={theme === "dark" ? "Use light theme" : "Use dark theme"}
            onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
          >
            {theme === "dark" ? <Sun size={18} /> : <Moon size={18} />}
          </button>
        </header>
        <main class="content-area">{children}</main>
      </section>
    </div>
  );
}
