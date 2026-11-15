// Types of the Skep REST API, /api/v1. They come from api/openapi.yaml
// through openapi-typescript (npm run generate-types), which writes
// schema.d.ts; CI fails when that file is out of date (npm run
// check-types). This module gives the generated shapes short names.

import type { components, operations } from "./schema";

type Schemas = components["schemas"];
type OK<Op extends keyof operations> = operations[Op]["responses"] extends { 200: { content: { "application/json": infer T } } }
  ? T
  : never;

export type Capability = Schemas["Capability"];
export type CapabilityStatus = Capability["status"];
export type OSInfo = Schemas["OSInfo"];
export type BuildInfo = Schemas["BuildInfo"];
export type Platform = Schemas["Platform"];
export type System = Schemas["System"];
export type APIInfo = Schemas["APIInfo"];
export type TLSInfo = APIInfo["tls"];
export type Health = Schemas["Health"];
export type HealthCheck = Health["checks"][string];
export type HealthStatus = Health["status"];
export type Hardware = Schemas["Hardware"];
export type Bhyve = Schemas["Bhyve"];
export type QEMU = Schemas["QEMU"];
export type ZFS = Schemas["ZFS"];
export type ZFSPool = Schemas["ZFSPool"];

export type TaskStatus = Schemas["TaskStatus"];
export type Task = Schemas["Task"];
export type TaskEvent = Schemas["TaskEvent"];
export type TaskDetail = Schemas["TaskDetail"];
export type TaskList = Schemas["TaskList"];

export type User = Schemas["User"];
export type NewUser = Schemas["NewUser"];
export type UserUpdate = Schemas["UserUpdate"];
export type UserList = OK<"listUsers">;
export type Role = Schemas["Role"];
export type RoleList = OK<"listRoles">;
export type AuditEntry = Schemas["AuditEntry"];
export type AuditList = OK<"listAudit">;

export type LoginRequest = Schemas["LoginRequest"];
export type PasswordChange = Schemas["PasswordChange"];
export type SessionResponse = Schemas["SessionResponse"];
export type SessionInfo = SessionResponse["session"];

export type VM = Schemas["VM"];
export type VMState = Schemas["VMState"];
export type VMDisk = Schemas["VMDisk"];
export type VMNIC = Schemas["VMNIC"];
export type VMList = Schemas["VMList"];
export type VMCreate = Schemas["VMCreate"];
export type VMUpdate = Schemas["VMUpdate"];
export type VMCreated = Schemas["VMCreated"];
export type VMAction = Schemas["VMAction"];
export type VMActionName = VMAction["action"];
export type StoragePool = Schemas["StoragePool"];
export type StoragePoolList = Schemas["StoragePoolList"];
export type Media = Schemas["Media"];
export type MediaList = Schemas["MediaList"];
export type EngineCapabilities = Schemas["EngineCapabilities"];

export type Snapshot = Schemas["Snapshot"];
export type SnapshotList = Schemas["SnapshotList"];
export type SnapshotCreate = Schemas["SnapshotCreate"];
export type SnapshotCreated = Schemas["SnapshotCreated"];
export type SnapshotRollback = Schemas["SnapshotRollback"];
export type VMClone = Schemas["VMClone"];
export type Dataset = Schemas["Dataset"];
export type DatasetOwner = Schemas["DatasetOwner"];
export type DatasetList = Schemas["DatasetList"];
export type Metrics = Schemas["Metrics"];
export type PoolMetrics = Schemas["PoolMetrics"];
export type VMMetrics = Schemas["VMMetrics"];

export type ErrorResponse = Schemas["ErrorResponse"];
export type ErrorCode = ErrorResponse["error"]["code"];

/** Every task status, in lifecycle order, for filters. */
export const TASK_STATUSES: readonly TaskStatus[] = [
  "pending",
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
];

/** Permission strings the API knows (internal/auth/permissions.go). */
export const PERMISSIONS = {
  systemRead: "system:read",
  nodeRead: "node:read",
  taskRead: "task:read",
  taskCancel: "task:cancel",
  userRead: "user:read",
  userManage: "user:manage",
  roleRead: "role:read",
  auditRead: "audit:read",
  vmRead: "vm:read",
  vmWrite: "vm:write",
  vmDelete: "vm:delete",
  vmPower: "vm:power",
  vmConsole: "vm:console",
  storageRead: "storage:read",
  vmSnapshot: "vm:snapshot",
  all: "*",
} as const;

export type Permission = (typeof PERMISSIONS)[keyof typeof PERMISSIONS];
