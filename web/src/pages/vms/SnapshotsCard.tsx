import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Snapshot, type Task, type VM } from "@/api/types";
import { useSession } from "@/app/session";
import { InlineError } from "@/components/errors";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime } from "@/lib/format";

import { VM_NAME_RE, isActive, newerSnapshots, snapshotsUnsupported, vmErrorHint } from "./vm";

/**
 * What ADR 0010 decision 3 requires the interface to say wherever a clone
 * is made or shown.
 */
export const CLONE_IDENTITY_NOTE =
  "A clone is the same guest as its source. Skep gives it a new ID, name, and MAC addresses, but its hostname, SSH host keys, and machine ID (/etc/hostid, /etc/machine-id, or the Windows SID) are the source's until you change them inside the clone.";

const NAME_HELP = "1 to 63 characters: a letter or digit, then letters, digits, '.', '_' or '-'.";

function withHint(err: ApiError): ApiError {
  const hint = vmErrorHint(err.code);
  return hint ? Object.assign(err, { message: `${err.message} ${hint}` }) : err;
}

type Pending =
  | { kind: "create" }
  | { kind: "rollback"; snap: Snapshot }
  | { kind: "delete"; snap: Snapshot }
  | { kind: "clone"; snap?: Snapshot };

/**
 * The snapshots of a VM (ADR 0010 decision 2): the list, and taking,
 * rolling back, deleting, and cloning. The agent checks every rule again;
 * the buttons only leave out what would be refused anyway.
 */
export function SnapshotsCard({ vm, onTask }: { vm: VM; onTask: (t: Task) => void }) {
  const { can } = useSession();
  const snaps = useQuery(`vm:${vm.id}:snapshots`, (o) => api.snapshots(vm.id, o), { refreshMs: 5_000 });
  const [pending, setPending] = useState<Pending | undefined>();
  const [error, setError] = useState<ApiError | undefined>();
  const [busy, setBusy] = useState(false);

  const list = snaps.data?.items ?? [];
  const unsupported = snapshotsUnsupported(vm);
  const canSnapshot = can(PERMISSIONS.vmSnapshot) && vm.provisioned && !unsupported;
  const canClone = canSnapshot && can(PERMISSIONS.vmWrite);
  const stopped = !isActive(vm);

  const submit = async (fn: () => Promise<Task>) => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    try {
      onTask(await fn());
      setPending(undefined);
      snaps.reload();
    } catch (err) {
      setError(withHint(toApiError(err)));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="lg:col-span-2">
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle>Snapshots</CardTitle>
        <div className="flex gap-2">
          {canSnapshot ? (
            <Button size="sm" variant="outline" onClick={() => setPending({ kind: "create" })}>
              Take snapshot
            </Button>
          ) : null}
          {canClone ? (
            <Button size="sm" variant="outline" onClick={() => setPending({ kind: "clone" })}>
              Clone
            </Button>
          ) : null}
        </div>
      </CardHeader>
      <CardContent className="space-y-3 px-2">
        {unsupported ? (
          <Notice tone="info" className="mx-2">
            This VM has a disk in a directory pool, which has no snapshots or clones; only ZFS pools do.
          </Notice>
        ) : null}
        {error ? (
          <div className="px-2">
            <InlineError error={error} />
          </div>
        ) : null}
        {snaps.error && !snaps.data ? (
          <div className="px-2">
            <InlineError error={snaps.error} />
          </div>
        ) : null}
        {list.length === 0 && snaps.data ? <p className="px-2 text-sm text-muted-foreground">No snapshots.</p> : null}
        {list.length > 0 ? (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Taken</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Space</TableHead>
                <TableHead>Clones</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((s) => (
                <TableRow key={s.id}>
                  <TableCell>
                    <div className="font-medium">{s.name}</div>
                    {s.description ? <div className="text-xs text-muted-foreground">{s.description}</div> : null}
                    {!s.provisioned ? <Badge tone="warn">not provisioned</Badge> : null}
                  </TableCell>
                  <TableCell className="text-xs">{formatDateTime(s.created_at)}</TableCell>
                  <TableCell>
                    {s.consistency === "crash_consistent" ? (
                      <Badge tone="info" title="Taken while the VM ran: what a power cut would have left on disk.">
                        crash-consistent
                      </Badge>
                    ) : (
                      <Badge tone="ok" title="Taken while the VM was stopped.">
                        consistent
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">
                    {s.observed ? (s.observed.exists ? formatBytes(s.observed.used_bytes) : "missing") : "—"}
                  </TableCell>
                  <TableCell className="text-xs">{s.clones.length ? s.clones.map((c) => c.name).join(", ") : "—"}</TableCell>
                  <TableCell className="text-right whitespace-nowrap">
                    {canSnapshot && s.provisioned ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!stopped}
                        title={stopped ? undefined : "Stop the VM to roll it back."}
                        onClick={() => setPending({ kind: "rollback", snap: s })}
                      >
                        Roll back
                      </Button>
                    ) : null}
                    {canClone && s.provisioned ? (
                      <Button size="sm" variant="ghost" onClick={() => setPending({ kind: "clone", snap: s })}>
                        Clone
                      </Button>
                    ) : null}
                    {can(PERMISSIONS.vmSnapshot) ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="text-bad"
                        disabled={s.clones.length > 0}
                        title={s.clones.length > 0 ? "Linked clones depend on this snapshot." : undefined}
                        onClick={() => setPending({ kind: "delete", snap: s })}
                      >
                        Delete
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </CardContent>

      {pending?.kind === "create" ? (
        <CreateSnapshotDialog
          vm={vm}
          busy={busy}
          onClose={() => setPending(undefined)}
          onSubmit={(name, description) =>
            submit(async () => (await api.createSnapshot(vm.id, { name, description: description || undefined })).task)
          }
        />
      ) : null}
      {pending?.kind === "rollback" ? (
        <RollbackDialog
          vm={vm}
          snap={pending.snap}
          newer={newerSnapshots(list, pending.snap)}
          busy={busy}
          onClose={() => setPending(undefined)}
          onSubmit={(destroyNewer) =>
            submit(() => api.rollbackSnapshot(vm.id, pending.snap.id, destroyNewer ? { destroy_newer: true } : {}))
          }
        />
      ) : null}
      {pending?.kind === "delete" ? (
        <Dialog
          open
          onClose={() => setPending(undefined)}
          title={`Delete snapshot ${pending.snap.name}?`}
          description="The snapshot is destroyed on every disk of the VM, with its copy of the UEFI variables. The VM itself is not changed."
        >
          <DialogActions>
            <Button variant="outline" onClick={() => setPending(undefined)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={busy} onClick={() => submit(() => api.deleteSnapshot(vm.id, pending.snap.id))}>
              Delete snapshot
            </Button>
          </DialogActions>
        </Dialog>
      ) : null}
      {pending?.kind === "clone" ? (
        <CloneDialog vm={vm} snapshots={list.filter((s) => s.provisioned)} initial={pending.snap} onClose={() => setPending(undefined)} onTask={onTask} />
      ) : null}
    </Card>
  );
}

function CreateSnapshotDialog({
  vm,
  busy,
  onClose,
  onSubmit,
}: {
  vm: VM;
  busy: boolean;
  onClose: () => void;
  onSubmit: (name: string, description: string) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const bad = name !== "" && !VM_NAME_RE.test(name);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (name === "" || bad) return;
    onSubmit(name, description);
  };
  return (
    <Dialog open onClose={onClose} title={`Take a snapshot of ${vm.name}`} description="One atomic snapshot of every disk of the VM, with its UEFI variables.">
      <form onSubmit={submit} className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="snap-name">Name</Label>
          <Input id="snap-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={bad} autoFocus />
          <FieldText error={bad}>{bad ? NAME_HELP : "Unique among this VM's snapshots."}</FieldText>
        </div>
        <div className="space-y-1">
          <Label htmlFor="snap-desc">Description</Label>
          <Input id="snap-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
        </div>
        {isActive(vm) ? (
          <Notice tone="info">
            The VM is running, so the snapshot is crash-consistent: what a power cut would leave on disk. Stop the VM first for a
            consistent one.
          </Notice>
        ) : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || name === "" || bad}>
            Take snapshot
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}

function RollbackDialog({
  vm,
  snap,
  newer,
  busy,
  onClose,
  onSubmit,
}: {
  vm: VM;
  snap: Snapshot;
  newer: Snapshot[];
  busy: boolean;
  onClose: () => void;
  onSubmit: (destroyNewer: boolean) => void;
}) {
  const [confirm, setConfirm] = useState(false);
  const needsDestroy = newer.length > 0;
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Roll ${vm.name} back to ${snap.name}?`}
      description="Every disk returns to its contents at the snapshot, and the UEFI variables to their copy. Anything written since is lost."
    >
      {needsDestroy ? (
        <div className="space-y-3">
          <Notice tone="warn" title="Newer snapshots are destroyed">
            ZFS rolls back only to the latest snapshot. Going back to this one destroys{" "}
            {newer.map((s) => s.name).join(", ")}.
          </Notice>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />
            Destroy {newer.length === 1 ? "the newer snapshot" : `the ${newer.length} newer snapshots`}
          </label>
        </div>
      ) : null}
      <DialogActions>
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="destructive" disabled={busy || (needsDestroy && !confirm)} onClick={() => onSubmit(needsDestroy)}>
          Roll back
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function CloneDialog({
  vm,
  snapshots,
  initial,
  onClose,
  onTask,
}: {
  vm: VM;
  snapshots: Snapshot[];
  initial?: Snapshot;
  onClose: () => void;
  onTask: (t: Task) => void;
}) {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [snapId, setSnapId] = useState(initial?.id ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();
  const bad = name !== "" && !VM_NAME_RE.test(name);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (name === "" || bad || busy) return;
    setBusy(true);
    setError(undefined);
    try {
      const created = await api.cloneVM(vm.id, snapId ? { name, snapshot_id: snapId } : { name });
      onTask(created.task);
      navigate(`/vms/${created.vm.id}`);
    } catch (err) {
      setError(withHint(toApiError(err)));
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={`Clone ${vm.name}`} description="A linked clone: its disks share blocks with the snapshot, which cannot be deleted while the clone exists.">
      <form onSubmit={submit} className="space-y-3">
        <Notice tone="warn" title="The clone is the same guest" data-testid="clone-identity-note">
          {CLONE_IDENTITY_NOTE}
        </Notice>
        <div className="space-y-1">
          <Label htmlFor="clone-name">Name of the new VM</Label>
          <Input id="clone-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={bad} autoFocus />
          {bad ? <FieldText error>{NAME_HELP}</FieldText> : null}
        </div>
        <div className="space-y-1">
          <Label htmlFor="clone-snap">From</Label>
          <Select id="clone-snap" value={snapId} onChange={(e) => setSnapId(e.target.value)}>
            <option value="">A new snapshot, taken now and kept</option>
            {snapshots.map((s) => (
              <option key={s.id} value={s.id}>
                Snapshot {s.name}
              </option>
            ))}
          </Select>
        </div>
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || name === "" || bad}>
            Clone
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
