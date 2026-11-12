import { BrowserRouter, Route, Routes } from "react-router";

import { NAV_ITEMS } from "@/app/navigation";
import { ThemeProvider } from "@/app/theme";
import { AppShell } from "@/layouts/AppShell";
import { DashboardPage } from "@/pages/DashboardPage";
import { NodesPage } from "@/pages/NodesPage";
import { NotFoundPage } from "@/pages/NotFoundPage";
import { PlannedPage } from "@/pages/PlannedPage";
import { TaskDetailPage } from "@/pages/TaskDetailPage";
import { TasksPage } from "@/pages/TasksPage";

export function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route index element={<DashboardPage />} />
            <Route path="nodes" element={<NodesPage />} />
            <Route path="tasks" element={<TasksPage />} />
            <Route path="tasks/:id" element={<TaskDetailPage />} />
            {NAV_ITEMS.filter((item) => item.planned).map((item) => (
              <Route key={item.path} path={item.path.slice(1)} element={<PlannedPage item={item} />} />
            ))}
            <Route path="*" element={<NotFoundPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
