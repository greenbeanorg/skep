import { afterEach, describe, expect, it, vi } from "vitest";

import { api, ApiError, auditPath, BACKGROUND_HEADER, listen, request, tasksPath } from "./client";

function respond(status: number, body: unknown, headers: Record<string, string> = {}): ReturnType<typeof vi.fn> {
  const fn = vi.fn(
    async () =>
      new Response(status === 204 ? null : typeof body === "string" ? body : JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json", ...headers },
      }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

function lastInit(fn: ReturnType<typeof vi.fn>): RequestInit {
  const call = fn.mock.calls.at(-1) as [string, RequestInit] | undefined;
  if (!call) throw new Error("fetch was not called");
  return call[1];
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("request", () => {
  it("returns the decoded body and calls the versioned path with same-origin credentials", async () => {
    const fn = respond(200, { status: "ok" });
    await expect(request("/health")).resolves.toEqual({ status: "ok" });
    expect(fn).toHaveBeenCalledWith("/api/v1/health", expect.objectContaining({ method: "GET", credentials: "same-origin" }));
    const headers = lastInit(fn).headers as Record<string, string>;
    expect(headers.Accept).toBe("application/json");
    expect(headers[BACKGROUND_HEADER]).toBeUndefined();
  });

  it("marks background requests and reports user requests", async () => {
    const fn = respond(200, {});
    const onUserRequest = vi.fn();
    const stop = listen({ onUserRequest });
    await request("/system", { background: true });
    expect((lastInit(fn).headers as Record<string, string>)[BACKGROUND_HEADER]).toBe("1");
    expect(onUserRequest).not.toHaveBeenCalled();
    await request("/system");
    expect((lastInit(fn).headers as Record<string, string>)[BACKGROUND_HEADER]).toBeUndefined();
    expect(onUserRequest).toHaveBeenCalledTimes(1);
    stop();
  });

  it("sends JSON bodies with the content type the API insists on", async () => {
    const fn = respond(200, { user: {} });
    await api.login({ username: "andy", password: "pw" });
    const init = lastInit(fn);
    expect(init.method).toBe("POST");
    expect((init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(init.body).toBe(JSON.stringify({ username: "andy", password: "pw" }));
  });

  it("turns the error envelope into an ApiError with details and the request ID", async () => {
    respond(403, {
      error: { code: "forbidden", message: "your role does not allow this", request_id: "r1", details: { permission: "user:manage" } },
    });
    const err = await request("/users").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 403, code: "forbidden", requestId: "r1", details: { permission: "user:manage" } });
  });

  it("reads Retry-After on 429", async () => {
    respond(429, { error: { code: "rate_limited", message: "too many" } }, { "Retry-After": "7" });
    await expect(request("/auth/login", { method: "POST", body: {} })).rejects.toMatchObject({
      status: 429,
      code: "rate_limited",
      retryAfter: 7,
    });
  });

  it("handles a non-JSON error body", async () => {
    respond(502, "Bad Gateway", { "X-Request-ID": "gw1" });
    await expect(request("/system")).rejects.toMatchObject({ status: 502, code: "unexpected_response", requestId: "gw1" });
  });

  it("returns undefined for 204", async () => {
    respond(204, null);
    await expect(api.logout()).resolves.toBeUndefined();
  });

  it("reports an unreachable API as status 0 and tells the connectivity listener", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );
    const onOutcome = vi.fn();
    const stop = listen({ onOutcome });
    const err = await request("/system").catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 0, code: "network_error" });
    expect((err as ApiError).isNetwork).toBe(true);
    expect(onOutcome).toHaveBeenCalledWith("unreachable");
    stop();
  });

  it("reports 503 agent_unavailable as an outcome, and a success as ok", async () => {
    const onOutcome = vi.fn();
    const stop = listen({ onOutcome });
    respond(503, { error: { code: "agent_unavailable", message: "the host agent is not reachable" } });
    const err = await request("/system").catch((e: unknown) => e);
    expect((err as ApiError).isAgentUnavailable).toBe(true);
    expect(onOutcome).toHaveBeenLastCalledWith("agent_unavailable");
    respond(200, {});
    await request("/system");
    expect(onOutcome).toHaveBeenLastCalledWith("ok");
    stop();
  });

  it("tells the session listener about 401 unauthenticated, but not for the login", async () => {
    const onUnauthenticated = vi.fn();
    const stop = listen({ onUnauthenticated });
    respond(401, { error: { code: "unauthenticated", message: "authentication is required" } });
    await request("/system", { background: true }).catch(() => undefined);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
    expect(onUnauthenticated.mock.calls[0]?.[1]).toBe(true);
    // A wrong password is a 401 too, with another code.
    respond(401, { error: { code: "invalid_credentials", message: "no" } });
    await api.login({ username: "a", password: "b" }).catch(() => undefined);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
    // An expired session answering the login path itself never happens,
    // but a 401 unauthenticated there must not log the login page out.
    respond(401, { error: { code: "unauthenticated", message: "no" } });
    await api.login({ username: "a", password: "b" }).catch(() => undefined);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
    stop();
  });

  it("never follows a redirect", async () => {
    const fn = respond(200, {});
    await request("/system");
    expect(lastInit(fn).redirect).toBe("error");
  });
});

describe("paths", () => {
  it("encodes only what is set", () => {
    expect(tasksPath()).toBe("/tasks");
    expect(tasksPath({ status: "failed", limit: 25 })).toBe("/tasks?status=failed&limit=25");
    expect(tasksPath({ cursor: "a b" })).toBe("/tasks?cursor=a+b");
    expect(auditPath()).toBe("/audit");
    expect(auditPath({ limit: 50, cursor: "12" })).toBe("/audit?limit=50&cursor=12");
  });

  it("escapes IDs in paths", async () => {
    const fn = respond(200, {});
    await api.task("a/b");
    expect(fn).toHaveBeenCalledWith("/api/v1/tasks/a%2Fb", expect.anything());
  });
});
