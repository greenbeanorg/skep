// A fake API and a render helper for the container page tests.

import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router";
import { vi } from "vitest";

import { SessionProvider, useSession } from "@/app/session";
import { SystemProvider } from "@/app/system";

export type Reply = { status: number; body?: unknown } | ((body: unknown) => { status: number; body?: unknown });

export interface Call {
  key: string;
  body: unknown;
}

/** A system document with the containers block; rctl on unless told. */
export function systemDoc(rctl = true) {
  return {
    node_id: "n1",
    bhyve: { bridges: ["bridge0", "bridge1"] },
    containers: { rctl, max_cpu_percent: 400 },
  };
}

/**
 * Stubs fetch: the session answers with `permissions`, GET /system with
 * `systemDoc(rctl)`, and everything else from `replies` by "METHOD path"
 * (404 otherwise). Returns the calls made.
 */
export function fakeApi(permissions: string[], replies: Record<string, Reply>, rctl = true): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const key = `${init.method ?? "GET"} ${url}`;
      const body = typeof init.body === "string" ? JSON.parse(init.body) : undefined;
      calls.push({ key, body });
      if (key === "GET /api/v1/auth/session") {
        return Response.json({
          user: { id: "u1", username: "andy", display_name: "", role: "x", disabled: false, created_at: "", updated_at: "" },
          session: { kind: "cookie", created_at: "", expires_at: "2099-01-01T00:00:00Z", idle_timeout_seconds: 1800 },
          permissions,
        });
      }
      if (key === "GET /api/v1/system" && !replies[key]) return Response.json(systemDoc(rctl));
      const r = replies[key];
      const out = typeof r === "function" ? r(body) : (r ?? { status: 404, body: { error: { code: "not_found", message: "no" } } });
      return new Response(JSON.stringify(out.body ?? {}), { status: out.status, headers: { "Content-Type": "application/json" } });
    }),
  );
  return calls;
}

function WhenLoggedIn({ children }: { children: ReactNode }) {
  const { state } = useSession();
  return state.status === "authenticated" ? children : null;
}

/** Renders `ui` logged in, with the system query, at `path` under `route`. */
export function renderAt(ui: ReactNode, path = "/", route = "/") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <WhenLoggedIn>
          <SystemProvider enabled>
            <Routes>
              <Route path={route} element={ui} />
              <Route path="*" element={<div data-testid="elsewhere" />} />
            </Routes>
          </SystemProvider>
        </WhenLoggedIn>
      </SessionProvider>
    </MemoryRouter>,
  );
}
