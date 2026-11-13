// One shared poll of GET /system for the app shell and the pages that show
// the node (the dashboard, the node page, settings). It is the single
// cached resource of the interface; a data-fetching library is
// reconsidered when more are needed (ADR 0008, decision 8).

import { createContext, useContext, type ReactNode } from "react";

import { api } from "@/api/client";
import { useQuery, type Query } from "@/api/hooks";
import type { System } from "@/api/types";

const SystemContext = createContext<Query<System> | undefined>(undefined);

export const SYSTEM_REFRESH_MS = 15_000;

export function SystemProvider({ children, enabled }: { children: ReactNode; enabled: boolean }) {
  const query = useQuery(enabled ? "system" : null, (o) => api.system(o), { refreshMs: SYSTEM_REFRESH_MS });
  return <SystemContext.Provider value={query}>{children}</SystemContext.Provider>;
}

export function useSystem(): Query<System> {
  const ctx = useContext(SystemContext);
  if (!ctx) throw new Error("useSystem must be used inside SystemProvider");
  return ctx;
}
