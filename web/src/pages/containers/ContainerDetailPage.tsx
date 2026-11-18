import { useCallback, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Container, type ContainerActionName, type Task } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { ErrorNotice, Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Notice } from "@/components/ui/notice";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime } from "@/lib/format";

import { CONTAINER_CLONE_NOTE, RCTL_LIMITS, isActive } from "./container";
import { ContainerSnapshotsCard } from "./ContainerSnapshotsCard";
import { ContainerStateBadge } from "./ContainersPage";
import { withHint } from "./CreateContainerDialog";
import { EditContainerDialog } from "./EditContainerDialog";
import { LIMIT_HELP } from "./LimitsFields";
import { TaskLine } from "./TaskLine";
import { UsageCard } from "./UsageCard";

/** A key line shortened to its type and comment: "ssh-ed25519 … andy@merrick". */
export function sshKeySummary(line: string): string {
  const [type = "", , ...comment] = line.trim().split(/\s+/);
  return comment.length ? `${type} … ${comment.join(" ")}` : `${type} …`;
}

const ACTION_LABEL: Record<ContainerActionName, string> = { start: "Start", stop: "Stop", restart: "Restart" };

/** The power actions a jail's state allows; the agent decides. */
export function powerActions(c: Container): ContainerActionName[] {
  if (!c.provisioned) return [];
  switch (c.state) {
    case "running":
      return ["stop", "restart"];
    case "stopped":
    case "failed":
    case "unknown":
      return ["start"];
    default:
      return [];
  }
}

export function ContainerDetailPage() {
  const { id = "" } = useParams();
  const { can } = useSession();
  const system = useSystem();
  const navigate = useNavigate();
  const location = useLocation();
  const ct = useQuery(`container:${id}`, (o) => api.container(id, o), { refreshMs: 2_000 });
  const [task, setTask] = useState<Task | undefined>((location.state as { task?: Task } | null)?.task);
  const [error, setError] = useState<ApiError | undefined>();
  const [busy, setBusy] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [editing, setEditing] = useState(false);
  const [logTick, setLogTick] = useState(0);
  const reload = ct.reload;
  const onTaskDone = useCallback(() => {
    reload();
    setLogTick((n) => n + 1);
  }, [reload]);

  const run = async (fn: () => Promise<Task>) => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    try {
      setTask(await fn());
      ct.reload();
    } catch (err) {
      setError(withHint(toApiError(err)));
    } finally {
      setBusy(false);
    }
  };

  if (ct.loading) return <LoadingCards count={2} />;
  if (!ct.data) {
    return (
      <>
        <PageHeader title="Container" />
        {ct.error ? <ErrorNotice error={ct.error} onRetry={ct.reload} /> : null}
      </>
    );
  }
  const c = ct.data;
  const active = isActive(c);
  const caps = system.data?.containers;
  const hasRctlLimit = RCTL_LIMITS.some((k) => Boolean(c.limits[k as keyof typeof c.limits]));

  return (
    <>
      <PageHeader
        title={c.name}
        description={
          <span className="flex flex-wrap items-center gap-2">
            <ContainerStateBadge state={c.state} />
            {!c.provisioned ? <Badge tone="info">provisioning</Badge> : null}
            {c.desired_state === "running" && c.state === "stopped" ? <Badge tone="warn">should be running</Badge> : null}
            {c.state_detail ? <span>{c.state_detail}</span> : null}
          </span>
        }
        actions={<RefreshButton onClick={ct.reload} updatedAt={ct.updatedAt} />}
      />
      {ct.error ? <ErrorNotice error={ct.error} onRetry={ct.reload} stale /> : null}

      <div className="mb-4 flex flex-wrap items-center gap-2">
        {can(PERMISSIONS.containerPower)
          ? powerActions(c).map((a) => (
              <Button
                key={a}
                size="sm"
                variant={a === "start" ? "default" : "outline"}
                disabled={busy}
                onClick={() => run(() => api.containerAction(c.id, a))}
              >
                {ACTION_LABEL[a]}
              </Button>
            ))
          : null}
        {can(PERMISSIONS.containerWrite) && c.provisioned ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy || c.state !== "stopped"}
            title={c.state !== "stopped" ? "Stop the jail to edit it." : undefined}
            onClick={() => setEditing(true)}
          >
            Edit
          </Button>
        ) : null}
        {can(PERMISSIONS.containerDelete) ? (
          <Button
            size="sm"
            variant="ghost"
            className="text-bad"
            disabled={busy || active}
            title={active ? "Stop the jail to delete it." : undefined}
            onClick={() => setConfirmDelete(true)}
          >
            Delete
          </Button>
        ) : null}
      </div>
      {task ? (
        <div className="mb-4">
          <TaskLine key={task.id} task={task} onDone={onTaskDone} />
        </div>
      ) : null}
      {error ? (
        <div className="mb-4">
          <InlineError error={error} />
        </div>
      ) : null}

      {caps && !caps.rctl && hasRctlLimit ? (
        <Notice tone="warn" title="This jail cannot start while RACCT is off" className="mb-4">
          It has a memory, CPU, process, or file-table limit, which needs RACCT (kern.racct.enable). A start is refused with
          rctl_unavailable until RACCT is enabled, or the limits are removed.
        </Notice>
      ) : null}
      {c.cloned_from_snapshot_id ? (
        <Notice tone="info" title="A linked clone" className="mb-4" data-testid="clone-identity-note">
          {CONTAINER_CLONE_NOTE}
        </Notice>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Configuration</CardTitle>
          </CardHeader>
          <CardContent>
            <Facts>
              <Fact label="ID" mono>
                {c.id}
              </Fact>
              <Fact label="Hostname">{c.hostname}</Fact>
              <Fact label="Template">
                {c.template_name}
                {c.release ? <span className="text-muted-foreground"> ({c.release})</span> : null}
              </Fact>
              <Fact label="Pool">{c.pool}</Fact>
              {c.jid ? (
                <Fact label="JID" mono>
                  {c.jid}
                </Fact>
              ) : null}
              <Fact label="Desired state">{c.desired_state}</Fact>
              <Fact label="Started before">
                {c.started_once ? "Yes. Skep no longer writes into its tree: addressing, nameservers, and the SSH key are fixed." : "No"}
              </Fact>
              <Fact label="Options">
                {[c.options.sysvipc ? "System V IPC" : "", c.options.mlock ? "lock memory" : ""].filter(Boolean).join(", ") || "none"}
              </Fact>
              <Fact label="SSH key">
                {c.ssh_authorized_key ? (
                  <span className="font-mono text-xs break-all">{sshKeySummary(c.ssh_authorized_key)}</span>
                ) : (
                  "none"
                )}
              </Fact>
              {c.nameservers?.length ? <Fact label="Nameservers">{c.nameservers.join(", ")}</Fact> : null}
              {c.observed_at ? <Fact label="Observed">{formatDateTime(c.observed_at)}</Fact> : null}
              <Fact label="Created">{formatDateTime(c.created_at)}</Fact>
            </Facts>
          </CardContent>
        </Card>
        <LimitsCard c={c} />
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Network</CardTitle>
          </CardHeader>
          <CardContent className="px-2">
            {c.nics.length === 0 ? (
              <p className="px-2 text-sm text-muted-foreground">No network interface: the jail has only its loopback.</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>In the jail</TableHead>
                    <TableHead>Bridge</TableHead>
                    <TableHead>IPv4</TableHead>
                    <TableHead>MAC</TableHead>
                    <TableHead>Host end</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {c.nics.map((n) => (
                    <TableRow key={n.id}>
                      <TableCell className="font-mono text-xs">{n.interface}</TableCell>
                      <TableCell>{n.bridge}</TableCell>
                      <TableCell className="text-xs">
                        {n.ipv4.mode === "dhcp" ? (
                          "DHCP"
                        ) : (
                          <>
                            <span className="font-mono">{n.ipv4.address}</span>
                            {n.ipv4.gateway ? <span className="text-muted-foreground"> via {n.ipv4.gateway}</span> : null}
                          </>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{n.mac_address}</TableCell>
                      <TableCell className="font-mono text-xs">{n.host_interface ?? "—"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
            {c.started_once && c.nics.length > 0 ? (
              <p className="px-2 pt-2 text-xs text-muted-foreground">
                The addressing is what Skep wrote before the first start; the jail may have changed it since.
              </p>
            ) : null}
          </CardContent>
        </Card>
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <UsageCard c={c} />
        <ConsoleLogCard id={c.id} tick={logTick} />
        {c.provisioned ? <ContainerSnapshotsCard c={c} onTask={setTask} /> : null}
      </div>

      {editing ? (
        <EditContainerDialog
          c={c}
          onClose={() => setEditing(false)}
          onTask={(t) => {
            setTask(t);
            ct.reload();
          }}
        />
      ) : null}
      <Dialog
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete ${c.name}?`}
        description="The jail's dataset and every snapshot of it are destroyed. This cannot be undone. A jail whose snapshots have linked clones cannot be deleted until the clones are."
      >
        <DialogActions>
          <Button variant="outline" onClick={() => setConfirmDelete(false)}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={busy}
            onClick={async () => {
              setConfirmDelete(false);
              setBusy(true);
              setError(undefined);
              try {
                await api.deleteContainer(c.id);
                navigate("/containers");
              } catch (err) {
                setError(withHint(toApiError(err)));
              } finally {
                setBusy(false);
              }
            }}
          >
            Delete jail and snapshots
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

const LIMIT_SHORT: Record<keyof typeof LIMIT_HELP, string> = {
  memory: "soft: a resident-memory target, not a cap",
  locked_memory: "what mlock(2) may lock",
  cpu_percent: "approximate; 100 is one CPU",
  max_processes: "a fork past it fails",
  open_files: "descriptor-table slots, not open files",
  disk_quota: "ZFS refquota, includes what the root shares with its template",
};

function LimitsCard({ c }: { c: Container }) {
  const keys = Object.keys(LIMIT_SHORT) as (keyof typeof LIMIT_SHORT)[];
  const set = keys.filter((k) => Boolean(c.limits[k]));
  const value = (k: keyof typeof LIMIT_SHORT) => {
    const v = c.limits[k];
    if (typeof v !== "number") return String(v);
    return k === "memory" || k === "locked_memory" || k === "disk_quota" ? formatBytes(v) : String(v);
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle>Limits</CardTitle>
      </CardHeader>
      <CardContent>
        {set.length === 0 ? (
          <p className="text-sm text-muted-foreground">No limits.</p>
        ) : (
          <Facts>
            {set.map((k) => (
              <Fact key={k} label={LIMIT_HELP[k].label}>
                <span title={LIMIT_HELP[k].help}>
                  {value(k)}
                  <span className="block text-xs text-muted-foreground">{LIMIT_SHORT[k]}</span>
                </span>
              </Fact>
            ))}
          </Facts>
        )}
        <p className="mt-3 text-xs text-muted-foreground">RCTL limits apply from the next start; the disk quota at once.</p>
      </CardContent>
    </Card>
  );
}

function ConsoleLogCard({ id, tick }: { id: string; tick: number }) {
  const log = useQuery(`container:${id}:console:${tick}`, (o) => api.containerConsoleLog(id, o));
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle>Console log</CardTitle>
        <RefreshButton onClick={log.reload} updatedAt={log.updatedAt} />
      </CardHeader>
      <CardContent className="space-y-2">
        {log.error && !log.data ? <InlineError error={log.error} /> : null}
        {log.data ? (
          log.data.text === "" ? (
            <p className="text-sm text-muted-foreground">Empty. /etc/rc and /etc/rc.shutdown write here when the jail starts and stops.</p>
          ) : (
            <pre className="max-h-80 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap" data-testid="console-log">
              {log.data.text}
            </pre>
          )
        ) : null}
        <p className="text-xs text-muted-foreground">
          {log.data?.truncated ? "Only the last 64 KiB is shown. " : ""}The log is kept until the host reboots.
        </p>
      </CardContent>
    </Card>
  );
}
