import { lazy, useEffect } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { PERMISSIONS } from "@/api/types";
import { connectConnectivity } from "@/app/connectivity";
import { NAV_ITEMS } from "@/app/navigation";
import { SessionProvider, useSession } from "@/app/session";
import { SystemProvider } from "@/app/system";
import { ThemeProvider } from "@/app/theme";
import { AppShell } from "@/layouts/AppShell";
import { ForbiddenPage } from "@/pages/ForbiddenPage";
import { LoginPage } from "@/pages/LoginPage";
import { NotFoundPage } from "@/pages/NotFoundPage";
import { PlannedPage } from "@/pages/PlannedPage";

// Pages are split by route: each is fetched the first time it is shown
// (AppShell's PageBoundary shows a placeholder meanwhile). The shell, the
// login page and the small fallback pages stay in the main bundle.
const AccountPage = lazy(() => import("@/pages/AccountPage").then((m) => ({ default: m.AccountPage })));
const ContainerDetailPage = lazy(() => import("@/pages/containers/ContainerDetailPage").then((m) => ({ default: m.ContainerDetailPage })));
const ContainersPage = lazy(() => import("@/pages/containers/ContainersPage").then((m) => ({ default: m.ContainersPage })));
const DashboardPage = lazy(() => import("@/pages/DashboardPage").then((m) => ({ default: m.DashboardPage })));
const NodesPage = lazy(() => import("@/pages/NodesPage").then((m) => ({ default: m.NodesPage })));
const SettingsPage = lazy(() => import("@/pages/SettingsPage").then((m) => ({ default: m.SettingsPage })));
const StoragePage = lazy(() => import("@/pages/StoragePage").then((m) => ({ default: m.StoragePage })));
const TaskDetailPage = lazy(() => import("@/pages/TaskDetailPage").then((m) => ({ default: m.TaskDetailPage })));
const TasksPage = lazy(() => import("@/pages/TasksPage").then((m) => ({ default: m.TasksPage })));
const UsersPage = lazy(() => import("@/pages/UsersPage").then((m) => ({ default: m.UsersPage })));
const VMDetailPage = lazy(() => import("@/pages/vms/VMDetailPage").then((m) => ({ default: m.VMDetailPage })));
const VMsPage = lazy(() => import("@/pages/vms/VMsPage").then((m) => ({ default: m.VMsPage })));

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
            path="containers"
            element={
              <Guarded permission={PERMISSIONS.containerRead}>
                <ContainersPage />
              </Guarded>
            }
          />
          <Route
            path="containers/:id"
            element={
              <Guarded permission={PERMISSIONS.containerRead}>
                <ContainerDetailPage />
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
