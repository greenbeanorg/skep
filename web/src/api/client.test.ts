import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError, apiGet, tasksPath } from "./client";

function respond(status: number, body: unknown): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(typeof body === "string" ? body : JSON.stringify(body), { status })),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("apiGet", () => {
  it("returns the decoded body and calls the versioned path", async () => {
    respond(200, { status: "ok" });
    await expect(apiGet("/health")).resolves.toEqual({ status: "ok" });
    expect(fetch).toHaveBeenCalledWith("/api/v1/health", expect.objectContaining({ headers: { Accept: "application/json" } }));
  });

  it("turns the error envelope into an ApiError", async () => {
    respond(404, { error: { code: "not_found", message: "the requested resource does not exist", request_id: "r1" } });
    const err = await apiGet("/tasks/nope").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 404, code: "not_found", requestId: "r1" });
  });

  it("handles a non-JSON error body", async () => {
    respond(502, "Bad Gateway");
    await expect(apiGet("/system")).rejects.toMatchObject({ status: 502, code: "unexpected_response" });
  });

  it("reports an unreachable API as status 0", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );
    await expect(apiGet("/system")).rejects.toMatchObject({ status: 0, code: "network_error" });
  });
});

describe("tasksPath", () => {
  it("encodes only what is set", () => {
    expect(tasksPath()).toBe("/tasks");
    expect(tasksPath({ status: "failed", limit: 25 })).toBe("/tasks?status=failed&limit=25");
    expect(tasksPath({ cursor: "a b" })).toBe("/tasks?cursor=a+b");
  });
});
