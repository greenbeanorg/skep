import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { User } from "@/api/types";
import { SessionProvider, useSession } from "@/app/session";

import { DeleteUserDialog, ToggleUserDialog, userErrorMessage, USERNAME_RE } from "./dialogs";

const me: User = {
  id: "u1",
  username: "andy",
  display_name: "Andy",
  role: "admin",
  disabled: false,
  created_at: "2026-11-01T00:00:00Z",
  updated_at: "2026-11-01T00:00:00Z",
};
const bob: User = { ...me, id: "u2", username: "bob", display_name: "Bob", role: "viewer" };

function fakeApi(replies: Record<string, { status: number; body?: unknown }>) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const key = `${init.method ?? "GET"} ${url}`;
      calls.push(key);
      const r = replies[key] ?? { status: 404, body: { error: { code: "not_found", message: "no" } } };
      return new Response(r.status === 204 ? null : JSON.stringify(r.body ?? {}), {
        status: r.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return calls;
}

const sessionReply = {
  status: 200,
  body: {
    user: me,
    session: { kind: "cookie", created_at: "2026-11-13T12:00:00Z", expires_at: "2026-11-14T00:00:00Z", idle_timeout_seconds: 1800 },
    permissions: ["*"],
  },
};

/** Renders children once the fake session is authenticated. */
function WhenLoggedIn({ children }: { children: React.ReactNode }) {
  const { state } = useSession();
  return state.status === "authenticated" ? children : null;
}

afterEach(() => vi.unstubAllGlobals());

describe("DeleteUserDialog", () => {
  it("deletes only after the username is typed, then reports done", async () => {
    const calls = fakeApi({ "GET /api/v1/auth/session": sessionReply, "DELETE /api/v1/users/u2": { status: 204 } });
    const onDone = vi.fn();
    render(
      <SessionProvider>
        <WhenLoggedIn>
          <DeleteUserDialog user={bob} onClose={() => undefined} onDone={onDone} />
        </WhenLoggedIn>
      </SessionProvider>,
    );
    const button = await screen.findByRole("button", { name: "Delete user" });
    expect(button).toBeDisabled();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Type the username/), "bo");
    expect(button).toBeDisabled();
    await user.type(screen.getByLabelText(/Type the username/), "b");
    expect(button).toBeEnabled();
    await user.click(button);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(calls).toContain("DELETE /api/v1/users/u2");
  });

  it("shows a last_admin refusal as such and does not report done", async () => {
    fakeApi({
      "GET /api/v1/auth/session": sessionReply,
      "DELETE /api/v1/users/u1": { status: 409, body: { error: { code: "last_admin", message: "server text", request_id: "r9" } } },
    });
    const onDone = vi.fn();
    render(
      <SessionProvider>
        <WhenLoggedIn>
          <DeleteUserDialog user={me} onClose={() => undefined} onDone={onDone} />
        </WhenLoggedIn>
      </SessionProvider>,
    );
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Type the username/), "andy");
    await user.click(screen.getByRole("button", { name: "Delete user" }));
    await screen.findByText(/only enabled administrator/);
    expect(screen.getByText(/request r9/)).toBeInTheDocument();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("ToggleUserDialog", () => {
  it("disables with the sessions warning and sends disabled=true", async () => {
    const fetchCalls: RequestInit[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit = {}) => {
        fetchCalls.push(init);
        if (url.endsWith("/auth/session")) {
          return new Response(JSON.stringify(sessionReply.body), { status: 200, headers: { "Content-Type": "application/json" } });
        }
        return new Response(JSON.stringify({ ...bob, disabled: true }), { status: 200, headers: { "Content-Type": "application/json" } });
      }),
    );
    const onDone = vi.fn();
    render(
      <SessionProvider>
        <WhenLoggedIn>
          <ToggleUserDialog user={bob} onClose={() => undefined} onDone={onDone} />
        </WhenLoggedIn>
      </SessionProvider>,
    );
    await screen.findByText("Disabling ends every session of this user");
    // findByText does not wait for the dialog to be open: the text is in the
    // DOM as soon as React commits, but the <dialog> gets its open attribute
    // in Dialog's effect, which can run later under load. A closed dialog's
    // contents are inaccessible, so getByRole here was flaky; findByRole
    // waits until the button is accessible.
    const disable = await screen.findByRole("button", { name: "Disable" });
    const user = userEvent.setup();
    await user.click(disable);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    const put = fetchCalls.find((i) => i.method === "PUT");
    expect(put).toBeDefined();
    expect(JSON.parse(String(put?.body))).toEqual({ disabled: true });
  });
});

describe("rules", () => {
  it("matches the API's username pattern", () => {
    for (const ok of ["a", "andy", "a.b_c-d", "a1234567890123456789012345678901"]) expect(USERNAME_RE.test(ok)).toBe(true);
    for (const bad of ["", "Andy", "1andy", "a b", "a12345678901234567890123456789012", "-a"]) expect(USERNAME_RE.test(bad)).toBe(false);
  });

  it("explains the domain errors", () => {
    expect(userErrorMessage({ code: "last_admin" } as never)).toMatch(/only enabled administrator/);
    expect(userErrorMessage({ code: "username_taken" } as never)).toMatch(/already exists/);
    expect(userErrorMessage({ code: "internal_error" } as never)).toBeUndefined();
  });
});
