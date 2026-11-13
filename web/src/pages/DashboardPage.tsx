import { Link } from "react-router";

import { api } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Capability } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { ErrorNotice, Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { CapabilityBadge, HealthBadge, TaskStatusBadge } from "@/components/status";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Notice } from "@/components/ui/notice";
import { formatAgo, formatDateTime, formatDuration, shortId } from "@/lib/format";

function CapabilityRow({ name, detail, capability }: { name: string; detail: string; capability: Capability }) {
  return (
    <li className="flex items-start justify-between gap-4 py-2.5">
      <div className="min-w-0">
        <div className="text-sm font-medium">{name}</div>
        <div className="text-xs text-muted-foreground">{detail}</div>
        <div className="mt-1 text-xs text-foreground/80">{capability.reason}</div>
      </div>
      <CapabilityBadge capability={capability} />
    </li>
  );
}

export function DashboardPage() {
  const { can } = useSession();
  const system = useSystem();
  const canReadTasks = can(PERMISSIONS.taskRead);
  const tasks = useQuery(canReadTasks ? "tasks:recent" : null, (o) => api.tasks({ limit: 5 }, o), {
    refreshMs: 10_000,
  });

  const sys = system.data;
  // The health document rides along in the system document.
  const health = sys?.health;
  const degraded = health ? Object.entries(health.checks ?? {}).filter(([, c]) => c.status !== "ok") : [];
  const reload = () => {
    system.reload();
    tasks.reload();
  };

  return (
    <>
      <PageHeader
        title="Dashboard"
        description="This node, as its agent reports it."
        actions={<RefreshButton onClick={reload} updatedAt={system.updatedAt} />}
      />

      {system.error ? <ErrorNotice error={system.error} onRetry={reload} stale={sys !== undefined} /> : null}
      {system.loading ? <LoadingCards count={4} /> : null}

      {sys ? (
        <div className="grid gap-4 lg:grid-cols-2">
          {!sys.production_host ? (
            <div className="rounded-lg border border-warn/40 bg-warn-soft px-4 py-3 text-sm lg:col-span-2">
              <span className="font-medium text-warn">Not a production host.</span>{" "}
              {sys.platform === "freebsd"
                ? "The agent runs in development mode."
                : `The agent runs on ${sys.os.name} as a development build. Skep manages FreeBSD hosts; nothing here reflects real hypervisor or storage capability.`}
            </div>
          ) : null}

          <Card>
            <CardHeader>
              <div>
                <CardTitle>System</CardTitle>
                <CardDescription>Identity and software</CardDescription>
              </div>
              <HealthBadge status={health?.status} />
            </CardHeader>
            <CardContent>
              <Facts>
                <Fact label="Hostname">{sys.hostname}</Fact>
                <Fact label="Node ID" mono>
                  {sys.node_id}
                </Fact>
                <Fact label="Platform">{sys.platform}</Fact>
                <Fact label="Operating system">
                  {sys.os.name} {sys.os.release}
                </Fact>
                <Fact label="Kernel">{sys.os.kernel || "unknown"}</Fact>
                <Fact label="Architecture">{sys.architecture}</Fact>
                <Fact label="Skep">
                  {sys.skep.version}{" "}
                  <span className="font-mono text-xs text-muted-foreground">({sys.skep.commit})</span>
                </Fact>
                <Fact label="Agent uptime">
                  {formatDuration(sys.uptime_seconds)}
                  <span className="text-muted-foreground"> · started {formatDateTime(sys.started_at)}</span>
                </Fact>
              </Facts>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>Capabilities</CardTitle>
                <CardDescription>What this host can run, and why</CardDescription>
              </div>
            </CardHeader>
            <CardContent>
              <ul className="divide-y">
                <CapabilityRow name="bhyve" detail="Primary hypervisor" capability={sys.capabilities.bhyve} />
                <CapabilityRow
                  name="QEMU"
                  detail="Optional; emulation and image tools"
                  capability={sys.capabilities.qemu}
                />
                <CapabilityRow name="OpenZFS" detail="Primary storage" capability={sys.capabilities.zfs} />
              </ul>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>Health checks</CardTitle>
                <CardDescription>The agent's dependencies and skep-api's own checks</CardDescription>
              </div>
              <HealthBadge status={health?.status} />
            </CardHeader>
            <CardContent>
              {degraded.length > 0 ? (
                <Notice tone={health?.status === "unavailable" ? "bad" : "warn"} className="mb-3" title="Needs attention">
                  {degraded.map(([name, check]) => (
                    <p key={name}>
                      <span className="font-medium">{name}</span>
                      {check.error ? <>: {check.error}</> : null}
                    </p>
                  ))}
                </Notice>
              ) : null}
              {health ? (
                <ul className="divide-y text-sm">
                  {Object.entries(health.checks ?? {}).map(([name, check]) => (
                    <li key={name} className="flex items-center justify-between gap-4 py-2">
                      <span>
                        {name}
                        {check.critical ? <span className="ml-2 text-xs text-muted-foreground">critical</span> : null}
                      </span>
                      <HealthBadge status={check.status} />
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-sm text-muted-foreground">No health document in the system response.</p>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <CardTitle>Recent tasks</CardTitle>
                <CardDescription>The last five operations of the agent</CardDescription>
              </div>
              {canReadTasks ? (
                <Link to="/tasks" className="text-xs font-medium underline underline-offset-2">
                  All tasks
                </Link>
              ) : null}
            </CardHeader>
            <CardContent>
              {!canReadTasks ? (
                <p className="text-sm text-muted-foreground">Your role cannot read tasks.</p>
              ) : tasks.error && !tasks.data ? (
                <p className="text-sm text-bad">{tasks.error.message}</p>
              ) : tasks.data && tasks.data.items.length === 0 ? (
                <p className="text-sm text-muted-foreground">No tasks yet.</p>
              ) : tasks.data ? (
                <ul className="divide-y text-sm">
                  {tasks.data.items.map((t) => (
                    <li key={t.id} className="flex items-center justify-between gap-4 py-2">
                      <Link to={`/tasks/${t.id}`} className="min-w-0 truncate hover:underline" title={t.id}>
                        <span className="font-mono text-[0.8125rem]">{t.type}</span>
                        <span className="ml-2 text-xs text-muted-foreground">
                          {shortId(t.id)} · {formatAgo(t.created_at)}
                        </span>
                      </Link>
                      <TaskStatusBadge status={t.status} />
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-sm text-muted-foreground">Loading…</p>
              )}
            </CardContent>
          </Card>
        </div>
      ) : null}
    </>
  );
}
