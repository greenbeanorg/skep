import { useEffect, useRef, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";

import { PageBoundary } from "@/components/page-boundary";

import { useConnectivity } from "@/app/connectivity";
import { visibleNav } from "@/app/navigation";
import { sessionWarning, useIdentity, useSession, type SessionWarning } from "@/app/session";
import { useSystem } from "@/app/system";
import { useTheme } from "@/app/theme";
import {
  ChevronDownIcon,
  KeyIcon,
  LogoutIcon,
  MenuIcon,
  MoonIcon,
  SkepMark,
  SunIcon,
  UserIcon,
  WarningIcon,
} from "@/components/icons";
import { HealthBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";

function Sidebar({ onNavigate }: { onNavigate: () => void }) {
  const { can } = useSession();
  const groups = visibleNav(can);
  return (
    <nav aria-label="Sections" className="flex h-full flex-col gap-6 overflow-y-auto px-3 py-4">
      <Link to="/" onClick={onNavigate} className="flex items-center gap-2.5 px-2">
        <span className="grid size-8 place-items-center rounded-md bg-primary text-primary-foreground">
          <SkepMark className="size-5" />
        </span>
        <span className="text-base font-semibold tracking-tight text-foreground">Skep</span>
      </Link>

      {groups.map((group) => (
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
                      planned
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

/** The account menu: who is logged in, the own account, log out. */
function AccountMenu() {
  const { user } = useIdentity();
  const { logout } = useSession();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const doLogout = async () => {
    setBusy(true);
    try {
      await logout();
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="relative" ref={ref}>
      <Button
        variant="ghost"
        size="sm"
        className="gap-1.5"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        onClick={() => setOpen((o) => !o)}
      >
        <UserIcon />
        <span className="max-w-32 truncate">{user.display_name || user.username}</span>
        <ChevronDownIcon className="size-3.5 text-muted-foreground" />
      </Button>
      {open ? (
        <div
          role="menu"
          className="absolute right-0 z-30 mt-1 w-60 rounded-md border bg-surface p-1 text-sm shadow-lg"
        >
          <div className="px-2 py-1.5">
            <div className="truncate font-medium">{user.display_name || user.username}</div>
            <div className="truncate text-xs text-muted-foreground">
              {user.username} · {user.role}
            </div>
          </div>
          <div className="my-1 border-t" />
          <Link
            role="menuitem"
            to="/account"
            onClick={() => setOpen(false)}
            className="flex items-center gap-2 rounded px-2 py-1.5 hover:bg-muted"
          >
            <KeyIcon className="size-4" />
            Your account
          </Link>
          <button
            role="menuitem"
            type="button"
            disabled={busy}
            onClick={doLogout}
            className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left hover:bg-muted disabled:opacity-50"
          >
            <LogoutIcon className="size-4" />
            {busy ? "Logging out…" : "Log out"}
          </button>
        </div>
      ) : null}
    </div>
  );
}

function TopBar({ onMenu }: { onMenu: () => void }) {
  const { theme, setChoice } = useTheme();
  const system = useSystem();
  const sys = system.data;
  const health = sys?.health;

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b bg-surface px-4">
      <Button variant="ghost" size="icon" className="lg:hidden" onClick={onMenu} aria-label="Open navigation">
        <MenuIcon />
      </Button>

      <div className="flex min-w-0 items-center gap-2.5">
        <span className="truncate text-sm font-medium">{sys ? sys.hostname : "this node"}</span>
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
        {health && health.status !== "ok" ? (
          <Link to="/" className="inline-flex" title="See the health checks on the dashboard">
            <HealthBadge status={health.status} />
          </Link>
        ) : (
          <HealthBadge status={health?.status} />
        )}
        <Button
          variant="ghost"
          size="icon"
          onClick={() => setChoice(theme === "dark" ? "light" : "dark")}
          aria-label={theme === "dark" ? "Switch to the light theme" : "Switch to the dark theme"}
        >
          {theme === "dark" ? <SunIcon /> : <MoonIcon />}
        </Button>
        <AccountMenu />
      </div>
    </header>
  );
}

/** Banners for an unreachable API or agent (ADR 0008, decision 5). */
function ConnectivityBanner() {
  const outcome = useConnectivity();
  if (outcome === "ok") return null;
  return (
    <div role="alert" className="flex items-center gap-2 border-b border-bad/40 bg-bad-soft px-4 py-2 text-sm">
      <WarningIcon className="size-4 shrink-0 text-bad" />
      {outcome === "unreachable" ? (
        <span>
          <span className="font-medium text-bad">skep-api cannot be reached.</span> Pages show the last data they
          received, and refresh keeps trying.
        </span>
      ) : (
        <span>
          <span className="font-medium text-bad">The host agent is not reachable.</span> skep-api answers, but
          skep-agent does not. Pages show the last data they received.
        </span>
      )}
    </div>
  );
}

function describeWarning(w: SessionWarning, expiresAt: string): { text: string; extend: boolean } {
  const minutes = Math.max(1, Math.ceil(w.remainingMs / 60_000));
  if (w.kind === "idle") {
    return {
      text: `Your session ends in about ${minutes} minute${minutes === 1 ? "" : "s"} without activity.`,
      extend: true,
    };
  }
  return {
    text: `Your session reaches its fixed end at ${formatDateTime(expiresAt)} and cannot be extended. Log in again to continue.`,
    extend: false,
  };
}

/** The notice before the session ends (ADR 0008, decision 3). */
function SessionNotice() {
  const identity = useIdentity();
  const { refresh } = useSession();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 10_000);
    return () => window.clearInterval(id);
  }, []);
  const warning = sessionWarning(identity, now);
  if (!warning) return null;
  const { text, extend } = describeWarning(warning, identity.session.expires_at);
  return (
    <div role="status" className="flex flex-wrap items-center gap-3 border-b border-warn/40 bg-warn-soft px-4 py-2 text-sm">
      <WarningIcon className="size-4 shrink-0 text-warn" />
      <span>{text}</span>
      {extend ? (
        <Button variant="outline" size="sm" onClick={() => void refresh().catch(() => undefined)}>
          Stay logged in
        </Button>
      ) : null}
    </div>
  );
}

export function AppShell() {
  const [menuOpen, setMenuOpen] = useState(false);
  const location = useLocation();

  // Close the drawer on navigation and on Escape.
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
        <ConnectivityBanner />
        <SessionNotice />
        <main className="flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6">
            <PageBoundary resetKey={location.pathname}>
              <Outlet />
            </PageBoundary>
          </div>
        </main>
      </div>
    </div>
  );
}
