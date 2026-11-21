import type { ReactNode } from "react";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Container } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { Meter } from "@/components/page";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatBytes, formatDateTime, formatDuration, percent } from "@/lib/format";

import { limitNumber, metricsFor } from "./container";

/** GET /metrics names pools, VMs, and jails, so it needs all four reads. */
export const METRICS_PERMISSIONS = [
  PERMISSIONS.systemRead,
  PERMISSIONS.vmRead,
  PERMISSIONS.storageRead,
  PERMISSIONS.containerRead,
] as const;

function Row({ label, value, limit, pct }: { label: string; value: ReactNode; limit?: string; pct?: number | null }) {
  return (
    <div className="space-y-1">
      <div className="flex items-baseline justify-between gap-2 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular-nums">
          {value}
          {limit ? <span className="text-muted-foreground"> of {limit}</span> : null}
        </span>
      </div>
      {pct !== undefined ? <Meter value={pct === null ? null : Math.min(100, pct)} label={label} /> : null}
    </div>
  );
}

/**
 * A jail's current use from GET /metrics (ADR 0010 decision 15): the
 * root's space always, and the RACCT values while it runs on a host with
 * RACCT. Current values only; there is no history yet.
 */
export function UsageCard({ c }: { c: Container }) {
  const { can } = useSession();
  const system = useSystem();
  const allowed = METRICS_PERMISSIONS.every((p) => can(p));
  const metrics = useQuery(allowed ? "metrics" : null, (o) => api.metrics(o), { refreshMs: 10_000 });
  const m = metricsFor(metrics.data, c.id);
  const rctl = system.data?.containers?.rctl;

  const memLimit = limitNumber(c.limits.memory);
  const quota = limitNumber(c.limits.disk_quota);
  const procLimit = limitNumber(c.limits.max_processes);
  const fileLimit = limitNumber(c.limits.open_files);
  const cpuLimit = limitNumber(c.limits.cpu_percent);
  const hasRacct = m?.resident_memory_bytes !== undefined || m?.cpu_percent !== undefined || m?.processes !== undefined;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Usage</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3" data-testid="usage">
        {!allowed ? (
          <p className="text-sm text-muted-foreground">
            Usage comes from the node's metrics, which need system:read, vm:read, storage:read, and container:read.
          </p>
        ) : metrics.error && !metrics.data ? (
          <InlineError error={metrics.error} />
        ) : metrics.data && !m ? (
          <p className="text-sm text-muted-foreground">The latest sample has no values for this jail yet.</p>
        ) : m ? (
          <>
            {hasRacct ? (
              <>
                {m.cpu_percent !== undefined ? (
                  <Row
                    label="CPU (approximate)"
                    value={`${m.cpu_percent}%`}
                    limit={cpuLimit ? `${cpuLimit}%` : undefined}
                    pct={cpuLimit ? percent(m.cpu_percent, cpuLimit) : undefined}
                  />
                ) : null}
                {m.cpu_time_seconds !== undefined ? <Row label="CPU time" value={formatDuration(m.cpu_time_seconds)} /> : null}
                {m.resident_memory_bytes !== undefined ? (
                  <Row
                    label="Resident memory"
                    value={formatBytes(m.resident_memory_bytes)}
                    limit={memLimit ? `${formatBytes(memLimit)} (soft)` : undefined}
                    pct={memLimit ? percent(m.resident_memory_bytes, memLimit) : undefined}
                  />
                ) : null}
                {m.processes !== undefined ? (
                  <Row
                    label="Processes"
                    value={m.processes}
                    limit={procLimit ? String(procLimit) : undefined}
                    pct={procLimit ? percent(m.processes, procLimit) : undefined}
                  />
                ) : null}
                {m.open_file_slots !== undefined ? (
                  <Row
                    label="File-table slots"
                    value={m.open_file_slots}
                    limit={fileLimit ? String(fileLimit) : undefined}
                    pct={fileLimit ? percent(m.open_file_slots, fileLimit) : undefined}
                  />
                ) : null}
              </>
            ) : (
              <p className="text-sm text-muted-foreground">
                {rctl === false
                  ? "RACCT is off on this host, so only the disk use is known."
                  : "CPU, memory, process, and file-table use appear while the jail runs."}
              </p>
            )}
            <Row
              label="Disk referenced"
              value={formatBytes(m.disk_referenced_bytes)}
              limit={quota ? `${formatBytes(quota)} quota` : undefined}
              pct={quota ? percent(m.disk_referenced_bytes, quota) : undefined}
            />
            <Row label="Disk used, with snapshots" value={formatBytes(m.disk_used_bytes)} />
            {m.error ? <p className="text-xs text-warn">Not read: {m.error}</p> : null}
            {m.resident_memory_bytes !== undefined && memLimit && m.resident_memory_bytes > memLimit ? (
              <p className="text-xs text-muted-foreground">
                Over the memory limit: it is soft, so nothing is refused; the jail's pages are reclaimed first when the host runs short.
              </p>
            ) : null}
            <p className="text-xs text-muted-foreground">
              Sampled {formatDateTime(metrics.data?.observed_at)}. Referenced space includes what the root shares with its template.
            </p>
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
