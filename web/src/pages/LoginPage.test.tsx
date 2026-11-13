import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SessionProvider, useSession } from "@/app/session";
import { ThemeProvider } from "@/app/theme";

import { LoginPage } from "./LoginPage";

type Reply = { status: number; body?: unknown; headers?: Record<string, string> };

/** Answers fetch by method and path; anything unknown is a 404. */
function fakeApi(replies: Record<string, Reply | ((init: RequestInit) => Reply)>) {
  const calls: { key: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const key = `${init.method ?? "GET"} ${url}`;
      calls.push({ key, init });
      const r = replies[key];
      const reply = typeof r === "function" ? r(init) : (r ?? { status: 404, body: { error: { code: "not_found", message: "no" } } });
      return new Response(reply.status === 204 ? null : JSON.stringify(reply.body ?? {}), {
        status: reply.status,
        headers: { "Content-Type": "application/json", ...reply.headers },
      });
    }),
  );
  return calls;
}

const sessionBody = {
  user: { id: "u1", username: "andy", display_name: "Andy", role: "admin", disabled: false, created_at: "", updated_at: "" },
  session: { kind: "cookie", created_at: "2026-11-13T12:00:00Z", expires_at: "2026-11-14T00:00:00Z", idle_timeout_seconds: 1800 },
  permissions: ["*"],
};

function WhoAmI() {
  const { state } = useSession();
  return <div data-testid="state">{state.status === "authenticated" ? `in as ${state.user.username}` : state.status}</div>;
}

function Harness() {
  const { state } = useSession();
  return (
    <>
      <WhoAmI />
      {state.status === "anonymous" ? <LoginPage reason={state.reason} /> : null}
    </>
  );
}

function renderLogin() {
  return render(
    <ThemeProvider>
      <SessionProvider>
        <Harness />
      </SessionProvider>
    </ThemeProvider>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("LoginPage", () => {
  it("logs in with a cookie session and never asks for a token", async () => {
    const calls = fakeApi({
      "GET /api/v1/auth/session": { status: 401, body: { error: { code: "unauthenticated", message: "no" } } },
      "POST /api/v1/auth/login": { status: 200, body: sessionBody },
    });
    renderLogin();
    await screen.findByRole("form", { name: "Log in" });
    expect(screen.getByTestId("state")).toHaveTextContent("anonymous");

    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Username"), "andy");
    await user.type(screen.getByLabelText("Password"), "correct horse battery staple");
    await user.click(screen.getByRole("button", { name: "Log in" }));

    await waitFor(() => expect(screen.getByTestId("state")).toHaveTextContent("in as andy"));
    const login = calls.find((c) => c.key === "POST /api/v1/auth/login");
    expect(login).toBeDefined();
    const body = JSON.parse(String(login?.init.body)) as Record<string, unknown>;
    expect(body).toEqual({ username: "andy", password: "correct horse battery staple" });
    expect(body.session).toBeUndefined();
    expect(login?.init.credentials).toBe("same-origin");
    // Nothing about the session went to storage.
    expect(Object.keys(localStorage).filter((k) => k !== "skep-theme")).toEqual([]);
  });

  it("shows one message for a wrong username or password and clears the password", async () => {
    fakeApi({
      "GET /api/v1/auth/session": { status: 401, body: { error: { code: "unauthenticated", message: "no" } } },
      "POST /api/v1/auth/login": {
        status: 401,
        body: { error: { code: "invalid_credentials", message: "the username or password is not correct" } },
      },
    });
    renderLogin();
    await screen.findByRole("form", { name: "Log in" });
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Username"), "andy");
    await user.type(screen.getByLabelText("Password"), "wrong");
    await user.click(screen.getByRole("button", { name: "Log in" }));
    await screen.findByText("The username or password is not correct.");
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(screen.getByLabelText("Username")).toHaveValue("andy");
  });

  it("names the host command when no administrator exists", async () => {
    fakeApi({
      "GET /api/v1/auth/session": { status: 401, body: { error: { code: "unauthenticated", message: "no" } } },
      "POST /api/v1/auth/login": { status: 503, body: { error: { code: "setup_required", message: "no admin" } } },
    });
    renderLogin();
    await screen.findByRole("form", { name: "Log in" });
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Username"), "andy");
    await user.type(screen.getByLabelText("Password"), "whatever-long");
    await user.click(screen.getByRole("button", { name: "Log in" }));
    await screen.findByText("skep-api admin create --username NAME");
  });

  it("waits out a rate limit with the button disabled", async () => {
    fakeApi({
      "GET /api/v1/auth/session": { status: 401, body: { error: { code: "unauthenticated", message: "no" } } },
      "POST /api/v1/auth/login": {
        status: 429,
        body: { error: { code: "rate_limited", message: "too many" } },
        headers: { "Retry-After": "30" },
      },
    });
    renderLogin();
    await screen.findByRole("form", { name: "Log in" });
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Username"), "andy");
    await user.type(screen.getByLabelText("Password"), "whatever-long");
    await user.click(screen.getByRole("button", { name: "Log in" }));
    await screen.findByText(/Too many attempts\. Wait 30 seconds/);
    expect(screen.getByRole("button", { name: /Wait \d+s/ })).toBeDisabled();
  });

  it("explains why the login page is shown again", async () => {
    fakeApi({
      "GET /api/v1/auth/session": { status: 401, body: { error: { code: "unauthenticated", message: "no" } } },
    });
    render(
      <ThemeProvider>
        <SessionProvider>
          <LoginPage reason="expired" />
        </SessionProvider>
      </ThemeProvider>,
    );
    expect(screen.getByText("Your session has ended")).toBeInTheDocument();
  });
});
