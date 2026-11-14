import { useState, type FormEvent } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import type { VMCreate, VMCreated } from "@/api/types";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";
import { formatBytes } from "@/lib/format";

import { VM_NAME_RE, parseSize, sizeProblem, vmErrorHint } from "./vm";

const MIN_MEMORY = 128 << 20;
const MIN_DISK = 16 << 20;

/**
 * The create form: one disk, optional install media, and one NIC, which
 * is what the first VM needs. The API takes more (up to 8 disks and 4
 * NICs), and skepctl can send it.
 */
export function CreateVMDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (c: VMCreated) => void }) {
  const system = useSystem();
  const pools = useQuery("storage:pools", (o) => api.storagePools(o));
  const media = useQuery("storage:media", (o) => api.media(o));
  const bridges = system.data?.bhyve?.bridges ?? [];
  const maxCPUs = system.data?.bhyve?.max_vcpus_per_vm || 1;

  const [name, setName] = useState("");
  const [cpus, setCpus] = useState("2");
  const [memory, setMemory] = useState("1G");
  const [diskSize, setDiskSize] = useState("8G");
  const [pool, setPool] = useState("");
  const [thin, setThin] = useState(true);
  const [iso, setIso] = useState("");
  const [bridge, setBridge] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();

  const poolNames = (pools.data?.items ?? []).map((p) => p.name);
  const chosenPool = pool || poolNames[0] || "";
  const chosenBridge = bridge || bridges[0] || "";
  const memBytes = parseSize(memory);
  const diskBytes = parseSize(diskSize);
  const cpuCount = Number(cpus);
  const problems = {
    name: name === "" || VM_NAME_RE.test(name) ? undefined : "1 to 63 characters: a letter or digit, then letters, digits, '.', '_' or '-'.",
    cpus: Number.isInteger(cpuCount) && cpuCount >= 1 && cpuCount <= maxCPUs ? undefined : `From 1 to ${maxCPUs} on this host.`,
    memory: sizeProblem(memBytes, MIN_MEMORY, "memory"),
    disk: sizeProblem(diskBytes, MIN_DISK, "disk size"),
  };
  const poolInfo = pools.data?.items.find((p) => p.name === chosenPool);
  const ready = name !== "" && !Object.values(problems).some(Boolean) && chosenPool !== "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    const req: VMCreate = {
      name,
      cpu_count: cpuCount,
      memory: memBytes ?? 0,
      disks: [{ size: diskBytes ?? 0, pool: chosenPool, thin }, ...(iso ? [{ media: iso }] : [])],
      nics: chosenBridge ? [{ bridge: chosenBridge }] : [],
    };
    setBusy(true);
    setError(undefined);
    try {
      onCreated(await api.createVM(req));
    } catch (err) {
      const apiErr = toApiError(err);
      const hint = vmErrorHint(apiErr.code);
      setError(hint ? Object.assign(apiErr, { message: `${apiErr.message} ${hint}` }) : apiErr);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title="Create a virtual machine"
      description="A bhyve guest with UEFI firmware, a disk on ZFS, and a serial console. Disks and network interfaces are fixed once it is created."
    >
      <form onSubmit={submit} className="space-y-4" noValidate>
        {pools.data && poolNames.length === 0 ? (
          <Notice tone="warn" title="No storage pool is allowed">
            The agent's configuration lists no pool in storage.pools, so no VM can be created on this host.
          </Notice>
        ) : null}
        <div className="space-y-1.5">
          <Label htmlFor="vm-name">Name</Label>
          <Input id="vm-name" value={name} onChange={(e) => setName(e.target.value)} required autoFocus disabled={busy} />
          {problems.name ? <FieldText error>{problems.name}</FieldText> : null}
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label htmlFor="vm-cpus">Virtual CPUs</Label>
            <Input id="vm-cpus" inputMode="numeric" value={cpus} onChange={(e) => setCpus(e.target.value)} disabled={busy} />
            {problems.cpus ? <FieldText error>{problems.cpus}</FieldText> : null}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="vm-memory">Memory</Label>
            <Input id="vm-memory" value={memory} onChange={(e) => setMemory(e.target.value)} disabled={busy} />
            {problems.memory ? <FieldText error>{problems.memory}</FieldText> : <FieldText>For example 512M or 2G.</FieldText>}
          </div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1.5">
            <Label htmlFor="vm-disk">Disk size</Label>
            <Input id="vm-disk" value={diskSize} onChange={(e) => setDiskSize(e.target.value)} disabled={busy} />
            {problems.disk ? <FieldText error>{problems.disk}</FieldText> : null}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="vm-pool">Pool</Label>
            <Select id="vm-pool" value={chosenPool} onChange={(e) => setPool(e.target.value)} disabled={busy || poolNames.length === 0}>
              {poolNames.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </Select>
            {poolInfo ? <FieldText>{formatBytes(poolInfo.available_bytes)} available</FieldText> : null}
          </div>
        </div>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" className="mt-0.5" checked={thin} onChange={(e) => setThin(e.target.checked)} disabled={busy} />
          <span>
            Thin provisioning
            <span className="block text-xs text-muted-foreground">
              No space is reserved. If the pool fills, the guest gets I/O errors. Without it, the full size is reserved now.
            </span>
          </span>
        </label>
        <div className="space-y-1.5">
          <Label htmlFor="vm-iso">Install media</Label>
          <Select id="vm-iso" value={iso} onChange={(e) => setIso(e.target.value)} disabled={busy}>
            <option value="">None</option>
            {(media.data?.items ?? []).map((m) => (
              <option key={m.name} value={m.name}>
                {m.name} ({formatBytes(m.size_bytes)})
              </option>
            ))}
          </Select>
          <FieldText>ISO files an administrator placed in the agent's media directory. It can be detached later.</FieldText>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="vm-bridge">Network</Label>
          <Select id="vm-bridge" value={chosenBridge} onChange={(e) => setBridge(e.target.value)} disabled={busy}>
            {bridges.map((b) => (
              <option key={b} value={b}>
                virtio-net on {b}
              </option>
            ))}
            <option value="">No network interface</option>
          </Select>
        </div>
        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" disabled={!ready || busy}>
            {busy ? "Creating…" : "Create"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
