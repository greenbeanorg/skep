import { describe, expect, it } from "vitest";

import type { SessionResponse } from "@/api/types";

import {
  ABSOLUTE_WARNING_MS,
  allows,
  IDLE_WARNING_MS,
  reduceSession,
  sessionWarning,
  type SessionState,
} from "./session";

const T0 = Date.parse("2026-11-13T12:00:00Z");

function response(overrides: Partial<SessionResponse["session"]> = {}): SessionResponse {
  return {
    user: {
      id: "u1",
      username: "andy",
      display_name: "Andy",
      role: "admin",
      disabled: false,
      created_at: "2026-11-01T00:00:00Z",
      updated_at: "2026-11-01T00:00:00Z",
    },
    session: {
      kind: "cookie",
      created_at: new Date(T0).toISOString(),
      expires_at: new Date(T0 + 12 * 3600_000).toISOString(),
      idle_timeout_seconds: 1800,
      ...overrides,
    },
    permissions: ["*"],
  };
}

function authenticated(overrides: Partial<SessionResponse["session"]> = {}): Extract<SessionState, { status: "authenticated" }> {
  const s = reduceSession({ status: "checking" }, { type: "authenticated", response: response(overrides), at: T0 });
  if (s.status !== "authenticated") throw new Error("not authenticated");
  return s;
}

describe("reduceSession", () => {
  it("moves from checking to authenticated and to anonymous with a reason", () => {
    const s = authenticated();
    expect(s.user.username).toBe("andy");
    expect(s.lastActivity).toBe(T0);
    const out = reduceSession(s, { type: "anonymous", reason: "expired" });
    expect(out).toEqual({ status: "anonymous", reason: "expired" });
  });

  it("records activity only forwards and only while authenticated", () => {
    const s = authenticated();
    const later = reduceSession(s, { type: "activity", at: T0 + 5000 });
    expect(later.status === "authenticated" && later.lastActivity).toBe(T0 + 5000);
    expect(reduceSession(later, { type: "activity", at: T0 + 1000 })).toBe(later);
    const anon: SessionState = { status: "anonymous", reason: undefined };
    expect(reduceSession(anon, { type: "activity", at: T0 })).toBe(anon);
  });

  it("keeps the later activity time when the identity is refreshed", () => {
    const s = reduceSession(authenticated(), { type: "activity", at: T0 + 60_000 });
    const refreshed = reduceSession(s, { type: "authenticated", response: response(), at: T0 + 30_000 });
    expect(refreshed.status === "authenticated" && refreshed.lastActivity).toBe(T0 + 60_000);
  });
});

describe("allows", () => {
  it("grants by name or by the asterisk", () => {
    expect(allows(["*"], "user:manage")).toBe(true);
    expect(allows(["system:read", "task:read"], "task:read")).toBe(true);
    expect(allows(["system:read"], "user:manage")).toBe(false);
    expect(allows([], "system:read")).toBe(false);
  });
});

describe("sessionWarning", () => {
  it("is silent while the session is fresh", () => {
    expect(sessionWarning(authenticated(), T0 + 60_000)).toBeUndefined();
  });

  it("warns two minutes before the idle timeout and counts down", () => {
    const s = authenticated();
    const idleEnd = T0 + 1800_000;
    expect(sessionWarning(s, idleEnd - IDLE_WARNING_MS - 1)).toBeUndefined();
    expect(sessionWarning(s, idleEnd - IDLE_WARNING_MS)).toEqual({ kind: "idle", remainingMs: IDLE_WARNING_MS });
    expect(sessionWarning(s, idleEnd - 30_000)).toEqual({ kind: "idle", remainingMs: 30_000 });
    expect(sessionWarning(s, idleEnd + 5_000)).toEqual({ kind: "idle", remainingMs: 0 });
  });

  it("activity pushes the idle warning back", () => {
    const s = reduceSession(authenticated(), { type: "activity", at: T0 + 600_000 });
    expect(s.status).toBe("authenticated");
    if (s.status !== "authenticated") return;
    expect(sessionWarning(s, T0 + 1800_000 - 60_000)).toBeUndefined();
    expect(sessionWarning(s, T0 + 600_000 + 1800_000 - 60_000)).toMatchObject({ kind: "idle" });
  });

  it("warns five minutes before the absolute end, which wins over idle when it comes first", () => {
    // Absolute end in 20 minutes, idle timeout 30 minutes: the absolute end
    // is what will happen.
    const s = authenticated({ expires_at: new Date(T0 + 20 * 60_000).toISOString() });
    expect(sessionWarning(s, T0 + 14 * 60_000)).toBeUndefined();
    expect(sessionWarning(s, T0 + 15 * 60_000)).toEqual({ kind: "absolute", remainingMs: ABSOLUTE_WARNING_MS });
    // Idle in 2 minutes, absolute in 4: both near; idle is sooner.
    const both = reduceSession(authenticated({ expires_at: new Date(T0 + 4 * 60_000 + 1800_000).toISOString() }), {
      type: "activity",
      at: T0,
    });
    if (both.status !== "authenticated") throw new Error();
    expect(sessionWarning(both, T0 + 1800_000 - 2 * 60_000)).toMatchObject({ kind: "idle" });
  });
});
