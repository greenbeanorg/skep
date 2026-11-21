// The one API client of the interface (ADR 0008, decision 5). No page calls
// fetch itself: every request goes through request(), which maps every
// error to an ApiError, tells the session state about a 401, tells the
// connectivity state about the agent and the network, and marks background
// requests so that they do not extend the session (decision 3).
//
// Requests use the browser's defaults for a same-origin fetch: the session
// cookie travels with credentials "same-origin", and the browser adds
// Origin to anything that is not GET or HEAD, which skep-api requires.
// Nothing about the session is kept here.

import type {
  AuditList,
  ErrorResponse,
  Hardware,
  LoginRequest,
  NewUser,
  PasswordChange,
  RoleList,
  SessionResponse,
  System,
  Task,
  TaskDetail,
  TaskList,
  TaskStatus,
  User,
  UserList,
  UserUpdate,
  MediaList,
  StoragePoolList,
  VM,
  VMActionName,
  VMCreate,
  VMCreated,
  VMList,
  VMUpdate,
  DatasetList,
  Metrics,
  SnapshotCreate,
  SnapshotCreated,
  SnapshotList,
  SnapshotRollback,
  VMClone,
  Container,
  ContainerActionName,
  ContainerClone,
  ContainerConsoleLog,
  ContainerCreate,
  ContainerCreated,
  ContainerList,
  ContainerUpdate,
  Template,
  TemplateList,
} from "./types";

export const API_PREFIX = "/api/v1";

/** The header that marks a request the interface made by itself. */
export const BACKGROUND_HEADER = "X-Skep-Background";

/** Client-side error codes, next to the API's. */
export const CODE_NETWORK = "network_error";
export const CODE_UNEXPECTED = "unexpected_response";

/** An error returned by the API, or a failure to reach it (status 0). */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId: string | undefined;
  readonly details: Record<string, unknown> | undefined;
  /** For 429: seconds to wait, from Retry-After. */
  readonly retryAfter: number | undefined;

  constructor(
    status: number,
    code: string,
    message: string,
    extra: { requestId?: string; details?: Record<string, unknown>; retryAfter?: number } = {},
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = extra.requestId;
    this.details = extra.details;
    this.retryAfter = extra.retryAfter;
  }

  /** The API could not be reached at all. */
  get isNetwork(): boolean {
    return this.status === 0;
  }

  /** The session is gone: expired, revoked, or logged out elsewhere. */
  get isUnauthenticated(): boolean {
    return this.status === 401 && this.code === "unauthenticated";
  }

  /** skep-api answers, but the host agent does not. */
  get isAgentUnavailable(): boolean {
    return this.status === 503 && this.code === "agent_unavailable";
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

/** Turns anything thrown into an ApiError, for display. */
export function toApiError(err: unknown): ApiError {
  if (err instanceof ApiError) return err;
  return new ApiError(0, "client_error", "The response could not be processed.");
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

export interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "DELETE";
  /** JSON body. */
  body?: unknown;
  signal?: AbortSignal;
  /**
   * The request was not caused by the user (a timer, a refetch on focus).
   * It carries the background header and is not counted as activity for
   * the idle warning.
   */
  background?: boolean;
}

/** What the client reports to whoever listens (the session and connectivity state). */
export interface ClientEvents {
  /** A request got 401 unauthenticated. The login endpoint never reports this. */
  onUnauthenticated: (error: ApiError, background: boolean) => void;
  /** Every finished request, for the connectivity banner. */
  onOutcome: (outcome: Outcome) => void;
  /** A request the user caused was sent, for the idle warning. */
  onUserRequest: (at: number) => void;
}

export type Outcome = "ok" | "agent_unavailable" | "unreachable";

const listeners: Partial<ClientEvents> = {};

/** Registers the listeners. Returns a function that removes them. */
export function listen(events: Partial<ClientEvents>): () => void {
  Object.assign(listeners, events);
  return () => {
    for (const key of Object.keys(events) as (keyof ClientEvents)[]) {
      if (listeners[key] === events[key]) delete listeners[key];
    }
  };
}

/** Performs one request against /api/v1 and returns the decoded body. */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (options.background) headers[BACKGROUND_HEADER] = "1";
  else listeners.onUserRequest?.(Date.now());

  let resp: Response;
  try {
    resp = await fetch(API_PREFIX + path, {
      method,
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
      credentials: "same-origin",
      redirect: "error",
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    listeners.onOutcome?.("unreachable");
    throw new ApiError(0, CODE_NETWORK, "skep-api cannot be reached.");
  }

  let body: unknown;
  if (resp.status !== 204) {
    const text = await resp.text();
    if (text !== "") {
      try {
        body = JSON.parse(text);
      } catch {
        body = undefined;
      }
    }
  }

  if (!resp.ok) {
    let error: ApiError;
    if (isErrorResponse(body)) {
      const retry = resp.headers.get("Retry-After");
      error = new ApiError(resp.status, body.error.code, body.error.message, {
        requestId: body.error.request_id,
        details: body.error.details,
        retryAfter: retry !== null && /^\d+$/.test(retry) ? Number(retry) : undefined,
      });
    } else {
      error = new ApiError(resp.status, CODE_UNEXPECTED, `The API answered with HTTP ${resp.status}.`, {
        requestId: resp.headers.get("X-Request-ID") ?? undefined,
      });
    }
    listeners.onOutcome?.(error.isAgentUnavailable ? "agent_unavailable" : "ok");
    if (error.isUnauthenticated && path !== "/auth/login") {
      listeners.onUnauthenticated?.(error, options.background === true);
    }
    throw error;
  }
  listeners.onOutcome?.("ok");
  if (resp.status === 204) return undefined as T;
  if (body === undefined) {
    throw new ApiError(resp.status, CODE_UNEXPECTED, "The API answered with something that is not JSON.", {
      requestId: resp.headers.get("X-Request-ID") ?? undefined,
    });
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

export interface AuditQuery {
  limit?: number;
  cursor?: string;
}

export function auditPath(q: AuditQuery = {}): string {
  const params = new URLSearchParams();
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  const qs = params.toString();
  return qs ? `/audit?${qs}` : "/audit";
}

export interface DatasetQuery {
  pool?: string;
  limit?: number;
  cursor?: string;
}

export function datasetsPath(q: DatasetQuery = {}): string {
  const params = new URLSearchParams();
  if (q.pool) params.set("pool", q.pool);
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  const qs = params.toString();
  return qs ? `/storage/datasets?${qs}` : "/storage/datasets";
}

/** Options of a read call. */
export interface ReadOptions {
  signal?: AbortSignal;
  background?: boolean;
}

const enc = encodeURIComponent;

/** Every endpoint of the API the interface uses, typed. */
export const api = {
  // Session (ADR 0008, decision 2). The login asks for a cookie session by
  // sending no "session" field; the token never reaches JavaScript.
  session: (o: ReadOptions = {}) => request<SessionResponse>("/auth/session", o),
  login: (req: LoginRequest) => request<SessionResponse>("/auth/login", { method: "POST", body: req }),
  logout: () => request<void>("/auth/logout", { method: "POST" }),
  changePassword: (req: PasswordChange) => request<void>("/auth/password", { method: "PUT", body: req }),

  // The node.
  system: (o: ReadOptions = {}) => request<System>("/system", o),
  hardware: (nodeId: string, o: ReadOptions = {}) => request<Hardware>(`/nodes/${enc(nodeId)}/hardware`, o),

  // Tasks.
  tasks: (q: TaskQuery = {}, o: ReadOptions = {}) => request<TaskList>(tasksPath(q), o),
  task: (id: string, o: ReadOptions = {}) => request<TaskDetail>(`/tasks/${enc(id)}`, o),
  cancelTask: (id: string) => request<Task>(`/tasks/${enc(id)}/cancel`, { method: "POST" }),

  // Users, roles, and the access audit log.
  users: (o: ReadOptions = {}) => request<UserList>("/users", o),
  user: (id: string, o: ReadOptions = {}) => request<User>(`/users/${enc(id)}`, o),
  createUser: (req: NewUser) => request<User>("/users", { method: "POST", body: req }),
  updateUser: (id: string, req: UserUpdate) => request<User>(`/users/${enc(id)}`, { method: "PUT", body: req }),
  deleteUser: (id: string) => request<void>(`/users/${enc(id)}`, { method: "DELETE" }),
  roles: (o: ReadOptions = {}) => request<RoleList>("/roles", o),
  audit: (q: AuditQuery = {}, o: ReadOptions = {}) => request<AuditList>(auditPath(q), o),

  // Virtual machines and the storage they may use (ADR 0009). Every write
  // answers with the task that carries it out.
  vms: (o: ReadOptions = {}) => request<VMList>("/vms?limit=200", o),
  vm: (id: string, o: ReadOptions = {}) => request<VM>(`/vms/${enc(id)}`, o),
  createVM: (req: VMCreate) => request<VMCreated>("/vms", { method: "POST", body: req }),
  updateVM: (id: string, req: VMUpdate) => request<Task>(`/vms/${enc(id)}`, { method: "PUT", body: req }),
  deleteVM: (id: string) => request<Task>(`/vms/${enc(id)}`, { method: "DELETE" }),
  vmAction: (id: string, action: VMActionName, timeout?: number) =>
    request<Task>(`/vms/${enc(id)}/actions`, { method: "POST", body: timeout ? { action, timeout } : { action } }),
  storagePools: (o: ReadOptions = {}) => request<StoragePoolList>("/storage/pools", o),
  media: (o: ReadOptions = {}) => request<MediaList>("/storage/media", o),

  // Snapshots, rollback, and linked clones (ADR 0010 decisions 2 and 3).
  snapshots: (vmId: string, o: ReadOptions = {}) => request<SnapshotList>(`/vms/${enc(vmId)}/snapshots`, o),
  createSnapshot: (vmId: string, req: SnapshotCreate) =>
    request<SnapshotCreated>(`/vms/${enc(vmId)}/snapshots`, { method: "POST", body: req }),
  rollbackSnapshot: (vmId: string, snapId: string, req: SnapshotRollback) =>
    request<Task>(`/vms/${enc(vmId)}/snapshots/${enc(snapId)}/rollback`, { method: "POST", body: req }),
  deleteSnapshot: (vmId: string, snapId: string) =>
    request<Task>(`/vms/${enc(vmId)}/snapshots/${enc(snapId)}`, { method: "DELETE" }),
  cloneVM: (vmId: string, req: VMClone) => request<VMCreated>(`/vms/${enc(vmId)}/clone`, { method: "POST", body: req }),

  // The storage inventory and current resource use (ADR 0010 decisions 1
  // and 15).
  datasets: (q: DatasetQuery = {}, o: ReadOptions = {}) => request<DatasetList>(datasetsPath(q), o),
  metrics: (o: ReadOptions = {}) => request<Metrics>("/metrics", o),

  // Jails and the templates they are made from (ADR 0010 decisions 7 to
  // 13). Every write answers with the task that carries it out.
  templates: (o: ReadOptions = {}) => request<TemplateList>("/storage/templates?limit=200", o),
  template: (id: string, o: ReadOptions = {}) => request<Template>(`/storage/templates/${enc(id)}`, o),
  containers: (o: ReadOptions = {}) => request<ContainerList>("/containers?limit=200", o),
  container: (id: string, o: ReadOptions = {}) => request<Container>(`/containers/${enc(id)}`, o),
  createContainer: (req: ContainerCreate) => request<ContainerCreated>("/containers", { method: "POST", body: req }),
  updateContainer: (id: string, req: ContainerUpdate) => request<Task>(`/containers/${enc(id)}`, { method: "PUT", body: req }),
  deleteContainer: (id: string) => request<Task>(`/containers/${enc(id)}`, { method: "DELETE" }),
  containerAction: (id: string, action: ContainerActionName) =>
    request<Task>(`/containers/${enc(id)}/actions`, { method: "POST", body: { action } }),
  containerConsoleLog: (id: string, o: ReadOptions = {}) =>
    request<ContainerConsoleLog>(`/containers/${enc(id)}/console-log`, o),
  containerSnapshots: (id: string, o: ReadOptions = {}) => request<SnapshotList>(`/containers/${enc(id)}/snapshots`, o),
  createContainerSnapshot: (id: string, req: SnapshotCreate) =>
    request<SnapshotCreated>(`/containers/${enc(id)}/snapshots`, { method: "POST", body: req }),
  rollbackContainerSnapshot: (id: string, snapId: string, req: SnapshotRollback) =>
    request<Task>(`/containers/${enc(id)}/snapshots/${enc(snapId)}/rollback`, { method: "POST", body: req }),
  deleteContainerSnapshot: (id: string, snapId: string) =>
    request<Task>(`/containers/${enc(id)}/snapshots/${enc(snapId)}`, { method: "DELETE" }),
  cloneContainer: (id: string, req: ContainerClone) =>
    request<ContainerCreated>(`/containers/${enc(id)}/clone`, { method: "POST", body: req }),
};

/**
 * The WebSocket address of a VM's serial console, on this origin. The
 * browser sends the session cookie and Origin with the handshake, which
 * skep-api checks like any other request.
 */
export function consoleURL(vmId: string, loc: Pick<Location, "protocol" | "host"> = window.location): string {
  const scheme = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${loc.host}${API_PREFIX}/vms/${enc(vmId)}/console/serial`;
}

/** Reports a keystroke in the console as user activity, for the idle warning. */
export function reportActivity(): void {
  listeners.onUserRequest?.(Date.now());
}
