import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Container, type ContainerClone, type Snapshot, type Task } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { formatBytes, formatDateTime } from "@/lib/format";

import { newerSnapshots } from "../vms/vm";
import {
  CONTAINER_CLONE_NOTE,
  CONTAINER_NAME_RE,
  NAME_HELP,
  cloneBlocked,
  isActive,
  nicProblem,
  nicsProblem,
  parseNameservers,
  toNICSpec,
  type NICForm,
} from "./container";
import { withHint } from "./CreateContainerDialog";
import { NICFields } from "./NICFields";

type Pending =
  | { kind: "create" }
  | { kind: "rollback"; snap: Snapshot }
  | { kind: "delete"; snap: Snapshot }
  | { kind: "clone"; snap?: Snapshot };

/**
 * The snapshots of a jail (ADR 0010 decision 2) and its clones (decision
 * 3, amendments j, k, l). The agent checks every rule again; the buttons
 * only leave out what would be refused anyway.
 */
export function ContainerSnapshotsCard({ c, onTask }: { c: Container; onTask: (t: Task) => void }) {
  const { can } = useSession();
  const snaps = useQuery(`container:${c.id}:snapshots`, (o) => api.containerSnapshots(c.id, o), { refreshMs: 5_000 });
  const [pending, setPending] = useState<Pending | undefined>();
  const [error, setError] = useState<ApiError | undefined>();
  const [busy, setBusy] = useState(false);

  const list = snaps.data?.items ?? [];
  const canSnapshot = can(PERMISSIONS.containerSnapshot) && c.provisioned;
  // A clone needs both (amendment j).
  const mayClone = canSnapshot && can(PERMISSIONS.containerWrite);
  const blocked = cloneBlocked(c);
  const stopped = !isActive(c);

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
          {mayClone ? (
            <Button size="sm" variant="outline" disabled={Boolean(blocked)} title={blocked} onClick={() => setPending({ kind: "clone" })}>
              Clone
            </Button>
          ) : null}
        </div>
      </CardHeader>
      <CardContent className="space-y-3 px-2">
        {mayClone && blocked ? (
          <Notice tone="info" title="Cannot be cloned" className="mx-2" data-testid="clone-blocked">
            {blocked}
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
                      <Badge tone="info" title="Taken while the jail ran: what a power cut would have left on disk.">
                        crash-consistent
                      </Badge>
                    ) : (
                      <Badge tone="ok" title="Taken while the jail was stopped.">
                        consistent
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">
                    {s.observed ? (s.observed.exists ? formatBytes(s.observed.used_bytes) : "missing") : "—"}
                  </TableCell>
                  <TableCell className="text-xs">{s.clones.length ? s.clones.map((x) => x.name).join(", ") : "—"}</TableCell>
                  <TableCell className="text-right whitespace-nowrap">
                    {canSnapshot && s.provisioned ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!stopped}
                        title={stopped ? undefined : "Stop the jail to roll it back."}
                        onClick={() => setPending({ kind: "rollback", snap: s })}
                      >
                        Roll back
                      </Button>
                    ) : null}
                    {mayClone && s.provisioned ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={Boolean(blocked)}
                        title={blocked}
                        onClick={() => setPending({ kind: "clone", snap: s })}
                      >
                        Clone
                      </Button>
                    ) : null}
                    {can(PERMISSIONS.containerSnapshot) ? (
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
          c={c}
          busy={busy}
          onClose={() => setPending(undefined)}
          onSubmit={(name, description) =>
            submit(async () => (await api.createContainerSnapshot(c.id, { name, description: description || undefined })).task)
          }
        />
      ) : null}
      {pending?.kind === "rollback" ? (
        <RollbackDialog
          c={c}
          snap={pending.snap}
          newer={newerSnapshots(list, pending.snap)}
          busy={busy}
          onClose={() => setPending(undefined)}
          onSubmit={(destroyNewer) =>
            submit(() => api.rollbackContainerSnapshot(c.id, pending.snap.id, destroyNewer ? { destroy_newer: true } : {}))
          }
        />
      ) : null}
      {pending?.kind === "delete" ? (
        <Dialog
          open
          onClose={() => setPending(undefined)}
          title={`Delete snapshot ${pending.snap.name}?`}
          description="The snapshot of the jail's root is destroyed. The jail itself is not changed, and may keep running."
        >
          <DialogActions>
            <Button variant="outline" onClick={() => setPending(undefined)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={busy} onClick={() => submit(() => api.deleteContainerSnapshot(c.id, pending.snap.id))}>
              Delete snapshot
            </Button>
          </DialogActions>
        </Dialog>
      ) : null}
      {pending?.kind === "clone" ? (
        <CloneDialog c={c} snapshots={list.filter((s) => s.provisioned)} initial={pending.snap} onClose={() => setPending(undefined)} onTask={onTask} />
      ) : null}
    </Card>
  );
}

function CreateSnapshotDialog({
  c,
  busy,
  onClose,
  onSubmit,
}: {
  c: Container;
  busy: boolean;
  onClose: () => void;
  onSubmit: (name: string, description: string) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const bad = name !== "" && !CONTAINER_NAME_RE.test(name);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (name === "" || bad) return;
    onSubmit(name, description);
  };
  return (
    <Dialog open onClose={onClose} title={`Take a snapshot of ${c.name}`} description="One ZFS snapshot of the jail's root. Memory is not saved.">
      <form onSubmit={submit} className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="snap-name">Name</Label>
          <Input id="snap-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={bad} autoFocus />
          <FieldText error={bad}>{bad ? NAME_HELP : "Unique among this jail's snapshots."}</FieldText>
        </div>
        <div className="space-y-1">
          <Label htmlFor="snap-desc">Description</Label>
          <Input id="snap-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
        </div>
        {isActive(c) ? (
          <Notice tone="info">
            The jail is running, so the snapshot is crash-consistent: what a power cut would leave on disk. Stop the jail first for a
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
  c,
  snap,
  newer,
  busy,
  onClose,
  onSubmit,
}: {
  c: Container;
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
      title={`Roll ${c.name} back to ${snap.name}?`}
      description="The jail's root returns to its contents at the snapshot. Anything written since is lost."
    >
      <div className="space-y-3">
        {needsDestroy ? (
          <>
            <Notice tone="warn" title="Newer snapshots are destroyed">
              ZFS rolls back only to the latest snapshot. Going back to this one destroys {newer.map((s) => s.name).join(", ")}.
            </Notice>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />
              Destroy {newer.length === 1 ? "the newer snapshot" : `the ${newer.length} newer snapshots`}
            </label>
          </>
        ) : null}
        {c.started_once ? (
          <p className="text-xs text-muted-foreground">
            The jail stays marked as started, even if the snapshot is older than its first start: Skep does not write into its tree again.
          </p>
        ) : null}
      </div>
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

/** The clone form's NIC entry for a source NIC: static ones need new addressing (amendment k). */
function cloneNICForm(n: Container["nics"][number]): NICForm {
  return { bridge: n.bridge, mode: n.ipv4.mode, address: "", gateway: "" };
}

function CloneDialog({
  c,
  snapshots,
  initial,
  onClose,
  onTask,
}: {
  c: Container;
  snapshots: Snapshot[];
  initial?: Snapshot;
  onClose: () => void;
  onTask: (t: Task) => void;
}) {
  const navigate = useNavigate();
  const system = useSystem();
  const bridges = system.data?.bhyve?.bridges ?? [];
  const [name, setName] = useState("");
  const [hostname, setHostname] = useState("");
  const [snapId, setSnapId] = useState(initial?.id ?? "");
  const [nics, setNics] = useState<NICForm[]>(() => c.nics.map(cloneNICForm));
  const [nameservers, setNameservers] = useState((c.nameservers ?? []).join(" "));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();

  const statics = c.nics.filter((n) => n.ipv4.mode === "static");
  const nsKept = statics.length > 0 && statics.length === c.nics.length;
  const ns = parseNameservers(nameservers);
  const problems = {
    name: name !== "" && !CONTAINER_NAME_RE.test(name) ? NAME_HELP : undefined,
    nics: nicsProblem(nics) ?? (nics.some((n, i) => c.nics[i]?.ipv4.mode === "static" && nicProblem(n)) ? "addressing" : undefined),
    nameservers: nsKept ? ns.problem : undefined,
  };
  const ready = name !== "" && !Object.values(problems).some(Boolean);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    const req: ContainerClone = {
      name,
      ...(hostname.trim() ? { hostname: hostname.trim() } : {}),
      ...(snapId ? { snapshot_id: snapId } : {}),
      // One entry per source NIC, in slot order; a DHCP NIC keeps its
      // bridge and addressing, a static one gets the new address.
      ...(statics.length ? { nics: c.nics.map((n, i) => (n.ipv4.mode === "static" ? toNICSpec(nics[i]!) : { bridge: n.bridge })) } : {}),
      ...(nsKept && ns.list.length ? { nameservers: ns.list } : {}),
    };
    setBusy(true);
    setError(undefined);
    try {
      const created = await api.cloneContainer(c.id, req);
      onTask(created.task);
      navigate(`/containers/${created.container.id}`, { state: { task: created.task } });
    } catch (err) {
      setError(withHint(toApiError(err)));
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      className="max-w-xl"
      title={`Clone ${c.name}`}
      description="A linked clone: its root shares blocks with the snapshot, which cannot be deleted while the clone exists, and neither can this jail. The clone gets the source's template, options, limits, SSH key, and interfaces, with new MAC addresses."
    >
      <form onSubmit={submit} className="space-y-3">
        <Notice tone="warn" title="The clone is the same guest" data-testid="clone-identity-note">
          {CONTAINER_CLONE_NOTE}
        </Notice>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor="clone-name">Name of the new jail</Label>
            <Input id="clone-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={Boolean(problems.name)} autoFocus />
            {problems.name ? <FieldText error>{problems.name}</FieldText> : null}
          </div>
          <div className="space-y-1">
            <Label htmlFor="clone-hostname">Hostname</Label>
            <Input id="clone-hostname" value={hostname} placeholder={name || "the name"} onChange={(e) => setHostname(e.target.value)} />
            <FieldText>Defaults to the clone's name.</FieldText>
          </div>
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
        {statics.length > 0 ? (
          <fieldset className="space-y-2" data-testid="clone-addressing">
            <legend className="text-sm font-medium">New addressing</legend>
            <FieldText>
              The source has a static address on {statics.map((n) => n.interface).join(", ")}. The clone would hold the same address,
              so each needs a new one. Skep writes it into the clone's rc.conf, which it can do because the source has never started.
            </FieldText>
            {c.nics.map((n, i) =>
              n.ipv4.mode === "static" ? (
                <NICFields
                  key={n.id}
                  slot={n.slot}
                  nic={nics[i]!}
                  bridges={bridges}
                  disabled={busy}
                  onChange={(next) => setNics(nics.map((x, j) => (j === i ? next : x)))}
                />
              ) : null,
            )}
            {nicsProblem(nics) ? <FieldText error>{nicsProblem(nics)}</FieldText> : null}
            {statics.length < c.nics.length ? (
              <FieldText>Interfaces with DHCP keep their bridge and DHCP.</FieldText>
            ) : null}
          </fieldset>
        ) : c.nics.length > 0 ? (
          <FieldText>Every interface uses DHCP, so the clone keeps the bridges and DHCP, with new MAC addresses.</FieldText>
        ) : null}
        {nsKept ? (
          <div className="space-y-1">
            <Label htmlFor="clone-ns">Nameservers</Label>
            <Input id="clone-ns" value={nameservers} onChange={(e) => setNameservers(e.target.value)} />
            <FieldText error={Boolean(problems.nameservers)} data-testid="clone-ns-note">
              {problems.nameservers ??
                "Inherited from source. Change them here for the clone; an empty field also keeps the source's."}
            </FieldText>
          </div>
        ) : null}
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={busy || !ready}>
            Clone
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
