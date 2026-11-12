import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { NAV } from "@/app/navigation";
import { useTheme } from "@/app/theme";
import { MenuIcon, MoonIcon, SkepMark, SunIcon } from "@/components/icons";
import { HealthBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

function Sidebar({ onNavigate }: { onNavigate: () => void }) {
  return (
    <nav aria-label="Sections" className="flex h-full flex-col gap-6 overflow-y-auto px-3 py-4">
      <Link to="/" onClick={onNavigate} className="flex items-center gap-2.5 px-2">
        <span className="grid size-8 place-items-center rounded-md bg-primary text-primary-foreground">
          <SkepMark className="size-5" />
        </span>
        <span className="text-base font-semibold tracking-tight text-foreground">Skep</span>
      </Link>

      {NAV.map((group) => (
        <div key={group.label}>
          <div className="px-2 pb-1.5 text-[0.6875rem] font-medium tracking-wider text-muted-foreground uppercase">
            {group.label}
          </div>
          <ul className="space-y-0.5">
            {group.items.map((item) => (
              <li key={item.path}>
                <NavLink
                  to={item.path}
                  end={item.path === "/"}
                  onClick={onNavigate}
                  className={({ isActive }) =>
                    cn(
                      "flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm transition-colors",
                      isActive
                        ? "bg-accent font-medium text-accent-foreground"
                        : "text-sidebar-foreground hover:bg-muted hover:text-foreground",
                    )
                  }
                >
                  <item.icon className="size-4 shrink-0" />
                  <span className="truncate">{item.label}</span>
                  {item.planned ? (
                    <span className="ml-auto text-[0.625rem] text-muted-foreground" title="Not built yet">
                      soon
                    </span>
                  ) : null}
                </NavLink>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  );
}

function TopBar({ onMenu }: { onMenu: () => void }) {
  const { theme, toggle } = useTheme();
  const system = useQuery("system", (signal) => api.system(signal), { refreshMs: 30_000 });
  const health = useQuery("health", (signal) => api.health(signal), { refreshMs: 10_000 });

  const sys = system.data;
  const unreachable = health.error !== undefined && health.data === undefined;

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b bg-surface px-4">
      <Button variant="ghost" size="icon" className="lg:hidden" onClick={onMenu} aria-label="Open navigation">
        <MenuIcon />
      </Button>

      <div className="flex min-w-0 items-center gap-2.5">
        <span className="truncate text-sm font-medium">{sys ? sys.hostname : "local node"}</span>
        {sys ? (
          <span className="hidden text-xs text-muted-foreground sm:inline">
            {sys.os.name} {sys.os.release}
          </span>
        ) : null}
        {sys && !sys.production_host ? (
          <Badge tone="warn" title="This node is not a production FreeBSD host">
            {sys.dev_mode ? "development mode" : "development build"}
          </Badge>
        ) : null}
      </div>

      <div className="ml-auto flex items-center gap-2">
        {unreachable ? <Badge tone="bad">API unreachable</Badge> : <HealthBadge status={health.data?.status} />}
        <Badge tone="neutral" title="The API has no authentication yet; this interface is read-only">
          read-only
        </Badge>
        <Button
          variant="ghost"
          size="icon"
          onClick={toggle}
          aria-label={theme === "dark" ? "Switch to the light theme" : "Switch to the dark theme"}
        >
          {theme === "dark" ? <SunIcon /> : <MoonIcon />}
        </Button>
      </div>
    </header>
  );
}

export function AppShell() {
  const [menuOpen, setMenuOpen] = useState(false);
  const location = useLocation();

  // Close the mobile drawer on navigation and on Escape.
  useEffect(() => setMenuOpen(false), [location.pathname]);
  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenuOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [menuOpen]);

  return (
    <div className="flex h-dvh overflow-hidden">
      <aside className="hidden w-60 shrink-0 border-r bg-sidebar lg:block">
        <Sidebar onNavigate={() => undefined} />
      </aside>

      {menuOpen ? (
        <div className="fixed inset-0 z-40 lg:hidden">
          <button
            type="button"
            className="absolute inset-0 bg-black/40"
            aria-label="Close navigation"
            onClick={() => setMenuOpen(false)}
          />
          <aside className="relative h-full w-64 border-r bg-sidebar shadow-xl">
            <Sidebar onNavigate={() => setMenuOpen(false)} />
          </aside>
        </div>
      ) : null}

      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar onMenu={() => setMenuOpen(true)} />
        <main className="flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
