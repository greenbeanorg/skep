import { useEffect } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { PERMISSIONS } from "@/api/types";
import { connectConnectivity } from "@/app/connectivity";
import { NAV_ITEMS } from "@/app/navigation";
import { SessionProvider, useSession } from "@/app/session";
import { SystemProvider } from "@/app/system";
import { ThemeProvider } from "@/app/theme";
import { AppShell } from "@/layouts/AppShell";
import { AccountPage } from "@/pages/AccountPage";
import { DashboardPage } from "@/pages/DashboardPage";
import { ForbiddenPage } from "@/pages/ForbiddenPage";
import { LoginPage } from "@/pages/LoginPage";
import { NodesPage } from "@/pages/NodesPage";
import { NotFoundPage } from "@/pages/NotFoundPage";
import { PlannedPage } from "@/pages/PlannedPage";
import { SettingsPage } from "@/pages/SettingsPage";
import { StoragePage } from "@/pages/StoragePage";
import { TaskDetailPage } from "@/pages/TaskDetailPage";
import { TasksPage } from "@/pages/TasksPage";
import { UsersPage } from "@/pages/UsersPage";
import { VMDetailPage } from "@/pages/vms/VMDetailPage";
import { VMsPage } from "@/pages/vms/VMsPage";

/** Renders a page only when the role allows it; the server still decides. */
function Guarded({ permission, children }: { permission: string; children: React.ReactNode }) {
  const { can } = useSession();
  return can(permission as Parameters<typeof can>[0]) ? children : <ForbiddenPage permission={permission} />;
}

function Routed() {
  const { state } = useSession();

  if (state.status === "checking") {
    return (
      <div className="grid min-h-dvh place-items-center text-sm text-muted-foreground" aria-busy="true">
        Checking your session…
      </div>
    );
  }
  if (state.status === "anonymous") {
    // Shown at the current address, so that a login returns to it.
    return <LoginPage reason={state.reason} />;
  }
  return (
    <SystemProvider enabled>
      <Routes>
        <Route path="login" element={<Navigate to="/" replace />} />
        <Route element={<AppShell />}>
          <Route
            index
            element={
              <Guarded permission={PERMISSIONS.systemRead}>
                <DashboardPage />
              </Guarded>
            }
          />
          <Route
            path="nodes"
            element={
              <Guarded permission={PERMISSIONS.nodeRead}>
                <NodesPage />
              </Guarded>
            }
          />
          <Route
            path="vms"
            element={
              <Guarded permission={PERMISSIONS.vmRead}>
                <VMsPage />
              </Guarded>
            }
          />
          <Route
            path="vms/:id"
            element={
              <Guarded permission={PERMISSIONS.vmRead}>
                <VMDetailPage />
              </Guarded>
            }
          />
          <Route
            path="storage"
            element={
              <Guarded permission={PERMISSIONS.storageRead}>
                <StoragePage />
              </Guarded>
            }
          />
          <Route
            path="tasks"
            element={
              <Guarded permission={PERMISSIONS.taskRead}>
                <TasksPage />
              </Guarded>
            }
          />
          <Route
            path="tasks/:id"
            element={
              <Guarded permission={PERMISSIONS.taskRead}>
                <TaskDetailPage />
              </Guarded>
            }
          />
          <Route
            path="users/*"
            element={
              <Guarded permission={PERMISSIONS.userRead}>
                <UsersPage />
              </Guarded>
            }
          />
          <Route
            path="settings"
            element={
              <Guarded permission={PERMISSIONS.systemRead}>
                <SettingsPage />
              </Guarded>
            }
          />
          <Route path="account" element={<AccountPage />} />
          {NAV_ITEMS.filter((item) => item.planned).map((item) => (
            <Route key={item.path} path={item.path.slice(1)} element={<PlannedPage item={item} />} />
          ))}
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </SystemProvider>
  );
}

export function App() {
  useEffect(() => connectConnectivity(), []);
  return (
    <ThemeProvider>
      <SessionProvider>
        <BrowserRouter>
          <Routed />
        </BrowserRouter>
      </SessionProvider>
    </ThemeProvider>
  );
}
