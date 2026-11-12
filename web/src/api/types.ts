// Types of the Skep REST API, /api/v1. They mirror pkg/models in the Go
// module and api/openapi.yaml. Keep the three in step.

export type CapabilityStatus = "available" | "unavailable" | "unknown";

export interface Capability {
  status: CapabilityStatus;
  reason: string;
}

export interface OSInfo {
  name: string;
  version: string;
  release: string;
  kernel: string;
}

export interface VersionInfo {
  version: string;
  commit: string;
  build_date: string;
  go_version: string;
  os: string;
  arch: string;
}

export type Platform = "freebsd" | "linux-dev" | "unsupported";

export interface System {
  node_id: string;
  hostname: string;
  platform: Platform | string;
  production_host: boolean;
  dev_mode: boolean;
  os: OSInfo;
  architecture: string;
  skep: VersionInfo;
  capabilities: { bhyve: Capability; qemu: Capability; zfs: Capability };
  started_at: string;
  uptime_seconds: number;
}

export interface HealthCheck {
  status: string;
  error?: string;
}

export interface Health {
  status: "ok" | "degraded" | string;
  version: string;
  uptime_seconds: number;
  checks: Record<string, HealthCheck>;
}

export interface BhyveInfo extends Capability {
  vmm_loaded: boolean;
  vmm_loadable: boolean;
  vmm_device?: string;
  binary_path?: string;
}

export interface QEMUBinary {
  name: string;
  path: string;
  version?: string;
  error?: string;
}

export interface QEMUInfo extends Capability {
  acceleration?: string;
  binaries: QEMUBinary[] | null;
}

export interface ZFSPool {
  name: string;
  size_bytes: number;
  allocated_bytes: number;
  free_bytes: number;
  fragmentation_percent: number | null;
  capacity_percent: number | null;
  health: string;
}

export interface ZFSInfo extends Capability {
  module_loaded: boolean;
  module_version?: string;
  zpool_path?: string;
  pools: ZFSPool[] | null;
  pools_error?: string;
}

export interface Hardware {
  node_id: string;
  platform: Platform | string;
  host: { hostname: string; os: OSInfo; architecture: string };
  cpu: { model: string; cores: number; threads: number };
  memory: { total_bytes: number; available_bytes: number };
  hypervisors: { bhyve: BhyveInfo; qemu: QEMUInfo };
  storage: { zfs: ZFSInfo };
  warnings: string[] | null;
  collected_at: string;
}

export type TaskStatus = "pending" | "running" | "succeeded" | "failed" | "cancelled" | "interrupted";

export const TASK_STATUSES: readonly TaskStatus[] = [
  "pending",
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
];

export interface Task {
  id: string;
  node_id: string;
  type: string;
  status: TaskStatus;
  resource_type?: string;
  resource_id?: string;
  actor: { id: string; name: string };
  request_id?: string;
  cancellable: boolean;
  cancel_requested_at?: string;
  result?: unknown;
  error?: { code: string; message: string };
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface TaskEvent {
  id: number;
  kind: "state" | "progress" | string;
  level: string;
  message: string;
  data?: unknown;
  created_at: string;
}

export interface TaskDetail extends Task {
  events: TaskEvent[] | null;
}

export interface TaskList {
  items: Task[];
  next_cursor?: string;
}

export interface ErrorResponse {
  error: {
    code: string;
    message: string;
    request_id?: string;
    details?: Record<string, unknown>;
  };
}
