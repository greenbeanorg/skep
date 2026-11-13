// The session state of the interface (ADR 0008, decisions 2, 3, and 5).
//
// The identity lives in memory only: on start the app asks GET
// /auth/session; 200 means logged in, 401 means the login page. Nothing
// about the session goes to browser storage, and the cookie is HttpOnly so
// JavaScript never sees it. A 401 from any later request ends the identity
// here and shows the login page with "Your session has ended", at the
// same address, so a login returns to it. Other tabs hear about a logout
// over a BroadcastChannel that carries only the word "logged out".

import { createContext, useCallback, useContext, useEffect, useMemo, useReducer, useRef, type ReactNode } from "react";

import { api, listen, type ApiError } from "@/api/client";
import type { Permission, SessionInfo, SessionResponse, User } from "@/api/types";

/** Why the login page is shown, when it is not the first visit. */
export type AnonymousReason = "expired" | "logged_out" | "logged_out_elsewhere" | "password_changed";

export type SessionState =
  | { status: "checking" }
  | { status: "anonymous"; reason: AnonymousReason | undefined }
  | {
      status: "authenticated";
      user: User;
      session: SessionInfo;
      permissions: readonly string[];
      /** When the identity was last confirmed or refreshed by the server. */
      confirmedAt: number;
      /** When the user last caused a request. For the idle warning. */
      lastActivity: number;
    };

export type SessionAction =
  | { type: "authenticated"; response: SessionResponse; at: number }
  | { type: "anonymous"; reason: AnonymousReason | undefined }
  | { type: "activity"; at: number };

export function reduceSession(state: SessionState, action: SessionAction): SessionState {
  switch (action.type) {
    case "authenticated":
      return {
        status: "authenticated",
        user: action.response.user,
        session: action.response.session,
        permissions: action.response.permissions,
        confirmedAt: action.at,
        lastActivity: state.status === "authenticated" ? Math.max(state.lastActivity, action.at) : action.at,
      };
    case "anonymous":
      return { status: "anonymous", reason: action.reason };
    case "activity":
      if (state.status !== "authenticated" || action.at <= state.lastActivity) return state;
      return { ...state, lastActivity: action.at };
  }
}

/** Whether a set of permissions grants one. An asterisk grants everything. */
export function allows(permissions: readonly string[], permission: Permission): boolean {
  return permissions.includes("*") || permissions.includes(permission);
}

export interface SessionContextValue {
  state: SessionState;
  /** Logs in for a cookie session. Throws an ApiError on failure. */
  login: (username: string, password: string) => Promise<void>;
  /** Ends the session on the server and here. */
  logout: () => Promise<void>;
  /** Asks the server for the identity again, as a user action. */
  refresh: () => Promise<void>;
  /** Marks the identity as gone, after the own password was changed. */
  passwordChanged: () => void;
  /** Whether the logged-in user's role has a permission. False when logged out. */
  can: (permission: Permission) => boolean;
}

const SessionContext = createContext<SessionContextValue | undefined>(undefined);

/** The channel name; the only message is the word "logged out". */
export const SESSION_CHANNEL = "skep-session";
const LOGGED_OUT = "logged out";

function openChannel(): BroadcastChannel | undefined {
  try {
    return typeof BroadcastChannel === "function" ? new BroadcastChannel(SESSION_CHANNEL) : undefined;
  } catch {
    return undefined;
  }
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, dispatch] = useReducer(reduceSession, { status: "checking" });
  const stateRef = useRef(state);
  stateRef.current = state;
  const channel = useRef<BroadcastChannel | undefined>(undefined);

  // Start: ask who is logged in.
  useEffect(() => {
    const controller = new AbortController();
    api
      .session({ signal: controller.signal })
      .then((res) => dispatch({ type: "authenticated", response: res, at: Date.now() }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // A 401 is the normal first visit. Anything else (the API is
        // down) also shows the login page; the login then reports what is
        // wrong.
        void err;
        dispatch({ type: "anonymous", reason: undefined });
      });
    return () => controller.abort();
  }, []);

  // What the API client reports.
  useEffect(() => {
    return listen({
      onUnauthenticated: (_error: ApiError) => {
        // During the start check the effect above decides. Afterwards a
        // 401 means the session ended: expired, revoked, or logged out
        // elsewhere.
        if (stateRef.current.status === "authenticated") dispatch({ type: "anonymous", reason: "expired" });
      },
      onUserRequest: (at) => dispatch({ type: "activity", at }),
    });
  }, []);

  // Other tabs.
  useEffect(() => {
    const ch = openChannel();
    channel.current = ch;
    if (!ch) return;
    ch.onmessage = (ev: MessageEvent) => {
      if (ev.data === LOGGED_OUT && stateRef.current.status === "authenticated") {
        dispatch({ type: "anonymous", reason: "logged_out_elsewhere" });
      }
    };
    return () => {
      ch.close();
      channel.current = undefined;
    };
  }, []);

  const tellOtherTabs = useCallback(() => {
    try {
      channel.current?.postMessage(LOGGED_OUT);
    } catch {
      // The other tabs find out at their next request.
    }
  }, []);

  const login = useCallback(async (username: string, password: string) => {
    const res = await api.login({ username, password });
    dispatch({ type: "authenticated", response: res, at: Date.now() });
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      // Whatever the server said, this tab is logged out. The server
      // logout is the thing that counts; a failed one shows the login
      // page here and the session ends by itself later.
      dispatch({ type: "anonymous", reason: "logged_out" });
      tellOtherTabs();
    }
  }, [tellOtherTabs]);

  const refresh = useCallback(async () => {
    const res = await api.session();
    dispatch({ type: "authenticated", response: res, at: Date.now() });
  }, []);

  const passwordChanged = useCallback(() => {
    dispatch({ type: "anonymous", reason: "password_changed" });
    tellOtherTabs();
  }, [tellOtherTabs]);

  const can = useCallback(
    (permission: Permission) => state.status === "authenticated" && allows(state.permissions, permission),
    [state],
  );

  const value = useMemo(
    () => ({ state, login, logout, refresh, passwordChanged, can }),
    [state, login, logout, refresh, passwordChanged, can],
  );
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error("useSession must be used inside SessionProvider");
  return ctx;
}

/** The logged-in identity. Only for components rendered when authenticated. */
export function useIdentity(): Extract<SessionState, { status: "authenticated" }> {
  const { state } = useSession();
  if (state.status !== "authenticated") throw new Error("useIdentity needs an authenticated session");
  return state;
}

/** Idle and absolute warnings (ADR 0008, decision 3). */
export interface SessionWarning {
  kind: "idle" | "absolute";
  /** Milliseconds until the session ends, as the interface computes it. */
  remainingMs: number;
}

export const IDLE_WARNING_MS = 2 * 60_000;
export const ABSOLUTE_WARNING_MS = 5 * 60_000;

/**
 * Computes the warning to show, if any. The server decides; this is a
 * courtesy and may be wrong by up to a minute, the server's touch
 * interval. The absolute end cannot be extended, so it takes precedence
 * once it is near.
 */
export function sessionWarning(
  state: Extract<SessionState, { status: "authenticated" }>,
  now: number,
): SessionWarning | undefined {
  const absoluteEnd = Date.parse(state.session.expires_at);
  const idleEnd = state.lastActivity + state.session.idle_timeout_seconds * 1000;
  if (Number.isFinite(absoluteEnd) && absoluteEnd - now <= ABSOLUTE_WARNING_MS && absoluteEnd <= idleEnd) {
    return { kind: "absolute", remainingMs: Math.max(0, absoluteEnd - now) };
  }
  if (idleEnd - now <= IDLE_WARNING_MS) {
    return { kind: "idle", remainingMs: Math.max(0, idleEnd - now) };
  }
  if (Number.isFinite(absoluteEnd) && absoluteEnd - now <= ABSOLUTE_WARNING_MS) {
    return { kind: "absolute", remainingMs: Math.max(0, absoluteEnd - now) };
  }
  return undefined;
}
