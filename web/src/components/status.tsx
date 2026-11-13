import type { Capability, TaskStatus } from "@/api/types";
import { Badge, type Tone } from "@/components/ui/badge";

function Dot() {
  return <span className="size-1.5 rounded-full bg-current" aria-hidden="true" />;
}

const TASK_TONE: Record<TaskStatus, Tone> = {
  pending: "neutral",
  running: "info",
  succeeded: "ok",
  failed: "bad",
  cancelled: "warn",
  interrupted: "warn",
};

export function TaskStatusBadge({ status }: { status: TaskStatus }) {
  return (
    <Badge tone={TASK_TONE[status] ?? "neutral"}>
      <Dot />
      {status}
    </Badge>
  );
}

const CAPABILITY_TONE: Record<string, Tone> = {
  available: "ok",
  unavailable: "neutral",
  unknown: "warn",
};

export function CapabilityBadge({ capability }: { capability: Capability }) {
  return (
    <Badge tone={CAPABILITY_TONE[capability.status] ?? "warn"}>
      <Dot />
      {capability.status}
    </Badge>
  );
}

const HEALTH_TONE: Record<string, Tone> = { ok: "ok", degraded: "warn", unavailable: "bad" };

export function HealthBadge({ status }: { status: string | undefined }) {
  if (status === undefined) return <Badge tone="neutral">checking</Badge>;
  return (
    <Badge tone={HEALTH_TONE[status] ?? "warn"}>
      <Dot />
      {status === "ok" ? "healthy" : status}
    </Badge>
  );
}

const POOL_TONE: Record<string, Tone> = {
  ONLINE: "ok",
  DEGRADED: "warn",
  FAULTED: "bad",
  UNAVAIL: "bad",
  OFFLINE: "neutral",
  REMOVED: "bad",
};

export function PoolHealthBadge({ health }: { health: string }) {
  return (
    <Badge tone={POOL_TONE[health] ?? "warn"}>
      <Dot />
      {health || "unknown"}
    </Badge>
  );
}
