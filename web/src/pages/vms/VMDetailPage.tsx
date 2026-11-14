import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Task, type VM, type VMActionName } from "@/api/types";
import { useSession } from "@/app/session";
import { InlineError } from "@/components/errors";
import { ErrorNotice, Fact, Facts, LoadingCards, PageHeader, RefreshButton } from "@/components/page";
import { TaskStatusBadge, VMStateBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime } from "@/lib/format";

import { SerialConsole } from "./SerialConsole";
import { hasConsole, isActive, vmErrorHint } from "./vm";

const ACTION_LABEL: Record<VMActionName, string> = {
  start: "Start",
  stop: "Stop",
  force_stop: "Force stop",
  restart: "Restart",
};

function withHint(err: ApiError): ApiError {
  const hint = vmErrorHint(err.code);
  return hint ? Object.assign(err, { message: `${err.message} ${hint}` }) : err;
}

/** The task of the last write from this page, followed until it ends. */
function TaskLine({ task }: { task: Task }) {
  const live = useQuery(`task:${task.id}`, (o) => api.task(task.id, o), { refreshMs: 1_000 });
  const t = live.data ?? task;
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm" role="status">
      <span className="font-mono text-[0.8125rem]">{t.type}</span>
      <TaskStatusBadge status={t.status} />
      {t.error ? <span className="text-muted-foreground">{vmErrorHint(t.error.code) ?? t.error.message}</span> : null}
      <Link to={`/tasks/${t.id}`} className="text-xs text-muted-foreground hover:underline">
        task details
      </Link>
    </div>
  );
}

export function VMDetailPage() {
  const { id = "" } = useParams();
  const { can } = useSession();
  const navigate = useNavigate();
  const vm = useQuery(`vm:${id}`, (o) => api.vm(id, o), { refreshMs: 2_000 });
  const [task, setTask] = useState<Task | undefined>();
  const [error, setError] = useState<ApiError | undefined>();
  const [busy, setBusy] = useState(false);
  const [showConsole, setShowConsole] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const run = async (fn: () => Promise<Task>) => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    try {
      setTask(await fn());
      vm.reload();
    } catch (err) {
      setError(withHint(toApiError(err)));
    } finally {
      setBusy(false);
    }
  };

  if (vm.loading) return <LoadingCards count={2} />;
  if (!vm.data) {
    return (
      <>
        <PageHeader title="Virtual machine" />
        {vm.error ? <ErrorNotice error={vm.error} onRetry={vm.reload} /> : null}
      </>
    );
  }
  const v: VM = vm.data;
  const active = isActive(v);
  const actions: VMActionName[] = !v.provisioned
    ? []
    : active || v.observed.state === "failed"
      ? v.observed.state === "running"
        ? ["stop", "restart", "force_stop"]
        : ["force_stop"]
      : ["start"];
  // A failed VM whose instance is gone can start again; one with an
  // instance left needs a force stop first. The agent decides; both are
  // offered.
  if (v.observed.state === "failed" && v.provisioned) actions.unshift("start");

  return (
    <>
      <PageHeader
        title={v.name}
        description={
          <span className="flex flex-wrap items-center gap-2">
            <VMStateBadge state={v.observed.state} />
            {!v.provisioned ? <Badge tone="info">provisioning</Badge> : null}
            {v.drift ? <Badge tone="warn">should be {v.desired_state}</Badge> : null}
            {v.observed.detail ? <span>{v.observed.detail}</span> : null}
          </span>
        }
        actions={<RefreshButton onClick={vm.reload} updatedAt={vm.updatedAt} />}
      />
      {vm.error ? <ErrorNotice error={vm.error} onRetry={vm.reload} stale /> : null}

      <div className="mb-4 flex flex-wrap items-center gap-2">
        {can(PERMISSIONS.vmPower)
          ? actions.map((a) => (
              <Button
                key={a}
                size="sm"
                variant={a === "force_stop" ? "destructive" : a === "start" ? "default" : "outline"}
                disabled={busy}
                onClick={() => run(() => api.vmAction(v.id, a))}
              >
                {ACTION_LABEL[a]}
              </Button>
            ))
          : null}
        {can(PERMISSIONS.vmConsole) && hasConsole(v) ? (
          <Button size="sm" variant="outline" onClick={() => setShowConsole((s) => !s)}>
            {showConsole ? "Hide console" : "Console"}
          </Button>
        ) : null}
        {can(PERMISSIONS.vmDelete) && !active ? (
          <Button size="sm" variant="ghost" className="text-bad" disabled={busy} onClick={() => setConfirmDelete(true)}>
            Delete
          </Button>
        ) : null}
      </div>
      {task ? (
        <div className="mb-4">
          <TaskLine key={task.id} task={task} />
        </div>
      ) : null}
      {error ? (
        <div className="mb-4">
          <InlineError error={error} />
        </div>
      ) : null}

      {showConsole && hasConsole(v) ? (
        <Card className="mb-4">
          <CardContent className="pt-4">
            <SerialConsole vmId={v.id} />
          </CardContent>
        </Card>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Configuration</CardTitle>
          </CardHeader>
          <CardContent>
            <Facts>
              <Fact label="ID" mono>
                {v.id}
              </Fact>
              <Fact label="Engine">
                {v.engine}, {v.firmware.toUpperCase()} firmware
              </Fact>
              <Fact label="CPUs">{v.cpu_count}</Fact>
              <Fact label="Memory">{formatBytes(v.memory_bytes)}</Fact>
              <Fact label="Desired state">{v.desired_state}</Fact>
              <Fact label="Observed">
                {v.observed.state}
                {v.observed.at ? <span className="text-muted-foreground"> at {formatDateTime(v.observed.at)}</span> : null}
              </Fact>
              {v.observed.pid ? (
                <Fact label="bhyve PID" mono>
                  {v.observed.pid}
                </Fact>
              ) : null}
              <Fact label="Created">{formatDateTime(v.created_at)}</Fact>
            </Facts>
          </CardContent>
        </Card>
        <MediaCard vm={v} busy={busy} canEdit={can(PERMISSIONS.vmWrite) && !active && v.provisioned} onChange={run} />
        <Card>
          <CardHeader>
            <CardTitle>Disks</CardTitle>
          </CardHeader>
          <CardContent className="px-2">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Slot</TableHead>
                  <TableHead>Bus</TableHead>
                  <TableHead>Size</TableHead>
                  <TableHead>Volume</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {v.disks
                  .filter((d) => d.bus !== "ahci-cd")
                  .map((d) => (
                    <TableRow key={d.id}>
                      <TableCell>{d.slot}</TableCell>
                      <TableCell className="font-mono text-xs">{d.bus}</TableCell>
                      <TableCell>
                        {formatBytes(d.size_bytes ?? 0)}
                        {d.thin ? <span className="text-xs text-muted-foreground"> thin</span> : null}
                      </TableCell>
                      <TableCell className="font-mono text-xs break-all">{d.dataset}</TableCell>
                    </TableRow>
                  ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Network</CardTitle>
          </CardHeader>
          <CardContent className="px-2">
            {v.nics.length === 0 ? (
              <p className="px-2 text-sm text-muted-foreground">No network interface.</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Slot</TableHead>
                    <TableHead>Model</TableHead>
                    <TableHead>MAC</TableHead>
                    <TableHead>Bridge</TableHead>
                    <TableHead>Tap</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {v.nics.map((n) => (
                    <TableRow key={n.id}>
                      <TableCell>{n.slot}</TableCell>
                      <TableCell className="font-mono text-xs">{n.model}</TableCell>
                      <TableCell className="font-mono text-xs">{n.mac_address}</TableCell>
                      <TableCell>{n.bridge}</TableCell>
                      <TableCell className="font-mono text-xs">{n.tap ?? "—"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>

      <Dialog
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete ${v.name}?`}
        description="The VM and its disks are destroyed. This cannot be undone. Install media files are not touched."
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
                await api.deleteVM(v.id);
                navigate("/vms");
              } catch (err) {
                setError(withHint(toApiError(err)));
              } finally {
                setBusy(false);
              }
            }}
          >
            Delete VM and disks
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

function MediaCard({
  vm,
  busy,
  canEdit,
  onChange,
}: {
  vm: VM;
  busy: boolean;
  canEdit: boolean;
  onChange: (fn: () => Promise<Task>) => void;
}) {
  const attached = vm.disks.filter((d) => d.bus === "ahci-cd").map((d) => d.media ?? "");
  const media = useQuery(canEdit ? "storage:media" : null, (o) => api.media(o));
  const [choice, setChoice] = useState("");
  const options = (media.data?.items ?? []).filter((m) => !attached.includes(m.name));
  return (
    <Card>
      <CardHeader>
        <CardTitle>Install media</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {attached.length === 0 ? (
          <p className="text-sm text-muted-foreground">None attached. The VM boots from its disk.</p>
        ) : (
          <ul className="space-y-1.5">
            {attached.map((name) => (
              <li key={name} className="flex items-center justify-between gap-2 text-sm">
                <span className="font-mono text-[0.8125rem] break-all">{name}</span>
                {canEdit ? (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => onChange(() => api.updateVM(vm.id, { media: attached.filter((n) => n !== name) }))}
                  >
                    Detach
                  </Button>
                ) : null}
              </li>
            ))}
          </ul>
        )}
        {canEdit && options.length > 0 ? (
          <div className="flex gap-2">
            <Select aria-label="Media to attach" value={choice} onChange={(e) => setChoice(e.target.value)} disabled={busy}>
              <option value="">Choose an ISO…</option>
              {options.map((m) => (
                <option key={m.name} value={m.name}>
                  {m.name}
                </option>
              ))}
            </Select>
            <Button
              size="sm"
              variant="outline"
              disabled={busy || choice === ""}
              onClick={() => {
                const name = choice;
                setChoice("");
                onChange(() => api.updateVM(vm.id, { media: [...attached, name] }));
              }}
            >
              Attach
            </Button>
          </div>
        ) : null}
        {!canEdit && isActive(vm) ? (
          <Notice tone="info">Media can be attached and detached while the VM is stopped.</Notice>
        ) : null}
      </CardContent>
    </Card>
  );
}
