// A small client for the read-only Skep API. There is no authentication
// yet (Milestone 4), so it sends no credentials and offers no write calls.

import type { ErrorResponse, Hardware, Health, System, TaskDetail, TaskList, TaskStatus } from "./types";

export const API_PREFIX = "/api/v1";

/** An error returned by the API, or a failure to reach it (status 0). */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

function isErrorResponse(v: unknown): v is ErrorResponse {
  if (typeof v !== "object" || v === null) return false;
  const e = (v as { error?: unknown }).error;
  return (
    typeof e === "object" &&
    e !== null &&
    typeof (e as { code?: unknown }).code === "string" &&
    typeof (e as { message?: unknown }).message === "string"
  );
}

/** GETs a path under /api/v1 and returns the decoded JSON body. */
export async function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(API_PREFIX + path, { headers: { Accept: "application/json" }, signal });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "network_error", "The Skep API could not be reached.");
  }

  let body: unknown;
  try {
    body = await resp.json();
  } catch {
    body = undefined;
  }

  if (!resp.ok) {
    if (isErrorResponse(body)) {
      throw new ApiError(resp.status, body.error.code, body.error.message, body.error.request_id);
    }
    throw new ApiError(resp.status, "unexpected_response", `The API answered with HTTP ${resp.status}.`);
  }
  if (body === undefined) {
    throw new ApiError(resp.status, "unexpected_response", "The API answered with something that is not JSON.");
  }
  return body as T;
}

export interface TaskQuery {
  status?: TaskStatus;
  limit?: number;
  cursor?: string;
}

/** Builds the path of the task list for a query. */
export function tasksPath(q: TaskQuery = {}): string {
  const params = new URLSearchParams();
  if (q.status) params.set("status", q.status);
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  const qs = params.toString();
  return qs ? `/tasks?${qs}` : "/tasks";
}

export const api = {
  health: (signal?: AbortSignal) => apiGet<Health>("/health", signal),
  system: (signal?: AbortSignal) => apiGet<System>("/system", signal),
  hardware: (nodeId: string, signal?: AbortSignal) =>
    apiGet<Hardware>(`/nodes/${encodeURIComponent(nodeId)}/hardware`, signal),
  tasks: (q?: TaskQuery, signal?: AbortSignal) => apiGet<TaskList>(tasksPath(q), signal),
  task: (id: string, signal?: AbortSignal) => apiGet<TaskDetail>(`/tasks/${encodeURIComponent(id)}`, signal),
};
