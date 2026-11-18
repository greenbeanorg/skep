import { useState, type FormEvent } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type Container, type ContainerOptions, type ContainerUpdate, type Task } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";

import {
  MAX_NICS,
  limitsToForm,
  mlockProblem,
  nameserversAllowed,
  newNIC,
  nicProblem,
  nicToForm,
  nicsProblem,
  parseLimits,
  parseNameservers,
  sshKeyProblem,
  startedNICSpecs,
  toNICSpec,
  type LimitsForm,
  type NICForm,
} from "./container";
import { withHint } from "./CreateContainerDialog";
import { LimitsFields } from "./LimitsFields";
import { NICFields } from "./NICFields";

function sameNICs(a: NICForm[], b: NICForm[]): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

/**
 * Changes a stopped jail through PUT /containers/{id}, limited to what it
 * supports: the hostname, the options, and the limits at any time (RCTL
 * limits from the next start, the disk quota at once); before the first
 * start also the NICs, nameservers, and SSH key, which are written into the
 * jail's tree; after it only each NIC's bridge (ADR 0010 decision 13).
 */
export function EditContainerDialog({ c, onClose, onTask }: { c: Container; onClose: () => void; onTask: (t: Task) => void }) {
  const { can } = useSession();
  const system = useSystem();
  const bridges = system.data?.bhyve?.bridges ?? [];
  const caps = system.data?.containers;
  const fresh = !c.started_once;
  // The SSH key needs a template with sshd (amendment m); listing it needs storage:read.
  const tpl = useQuery(fresh && can(PERMISSIONS.storageRead) ? `template:${c.template_id}` : null, (o) => api.template(c.template_id, o));

  const initialNICs = c.nics.map(nicToForm);
  const initialNS = (c.nameservers ?? []).join(" ");
  const [hostname, setHostname] = useState(c.hostname);
  const [options, setOptions] = useState<ContainerOptions>({ ...c.options });
  const [limitsForm, setLimitsForm] = useState<LimitsForm>(limitsToForm(c.limits));
  const [nics, setNics] = useState<NICForm[]>(initialNICs);
  const [nameservers, setNameservers] = useState(initialNS);
  const [sshKey, setSshKey] = useState(c.ssh_authorized_key ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();

  const { limits, problems: limitProblems } = parseLimits(limitsForm, caps);
  const nsAllowed = fresh && nameserversAllowed(nics);
  const ns = parseNameservers(nameservers);
  const withSSH = fresh && tpl.data?.has_sshd === true;
  const problems = {
    hostname: hostname.trim() === "" ? "Give a hostname." : hostname.length > 253 ? "At most 253 characters." : undefined,
    nics: nicsProblem(nics) ?? (nics.some((n) => nicProblem(n)) ? "addressing" : undefined),
    nameservers: nsAllowed ? ns.problem : undefined,
    sshKey: withSSH ? sshKeyProblem(sshKey) : undefined,
    mlock: mlockProblem(Boolean(options.mlock), limits, caps),
  };
  const ready = !Object.values(problems).some(Boolean) && !Object.values(limitProblems).some(Boolean);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    const nicsChanged = !sameNICs(nics, initialNICs);
    const req: ContainerUpdate = {
      hostname: hostname.trim(),
      options: { sysvipc: Boolean(options.sysvipc), mlock: Boolean(options.mlock) },
      // PUT replaces every limit: the full set is sent.
      limits,
    };
    if (nicsChanged) req.nics = fresh ? nics.map(toNICSpec) : startedNICSpecs(c, nics.map((n) => n.bridge));
    if (fresh) {
      const list = nsAllowed ? ns.list : [];
      if (list.join(" ") !== (c.nameservers ?? []).join(" ")) req.nameservers = list;
      if (withSSH && sshKey.trim() !== (c.ssh_authorized_key ?? "")) req.ssh_authorized_key = sshKey.trim();
    }
    setBusy(true);
    setError(undefined);
    try {
      onTask(await api.updateContainer(c.id, req));
      onClose();
    } catch (err) {
      setError(withHint(toApiError(err)));
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      className="max-w-2xl"
      title={`Edit ${c.name}`}
      description={
        fresh
          ? "The jail has never started, so its network, nameservers, and SSH key can still be written into its tree. The hostname and options take effect at the next start."
          : "The jail has started, so Skep no longer writes into its tree: its addressing, nameservers, and SSH key are fixed, and only each interface's bridge can change."
      }
    >
      <form onSubmit={submit} className="space-y-4" noValidate>
        <div className="space-y-1.5">
          <Label htmlFor="ed-hostname">Hostname</Label>
          <Input id="ed-hostname" value={hostname} onChange={(e) => setHostname(e.target.value)} disabled={busy} />
          {problems.hostname ? <FieldText error>{problems.hostname}</FieldText> : null}
        </div>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Network</legend>
          {fresh ? (
            <>
              {nics.length === 0 ? <p className="text-sm text-muted-foreground">No network interface.</p> : null}
              {nics.map((n, i) => (
                <NICFields
                  key={i}
                  slot={i}
                  nic={n}
                  bridges={bridges}
                  disabled={busy}
                  onChange={(next) => setNics(nics.map((x, j) => (j === i ? next : x)))}
                  onRemove={() => setNics(nics.filter((_, j) => j !== i))}
                />
              ))}
              {nicsProblem(nics) ? <FieldText error>{nicsProblem(nics)}</FieldText> : null}
              {bridges.length > 0 && nics.length < MAX_NICS ? (
                <Button type="button" size="sm" variant="outline" onClick={() => setNics([...nics, newNIC(bridges[0] ?? "")])} disabled={busy}>
                  Add network interface
                </Button>
              ) : null}
            </>
          ) : nics.length === 0 ? (
            <p className="text-sm text-muted-foreground">No network interface; none can be added after the first start.</p>
          ) : (
            nics.map((n, i) => (
              <div key={i} className="grid items-end gap-2 sm:grid-cols-2" data-testid={`started-nic-${i}`}>
                <div className="space-y-1">
                  <Label htmlFor={`ed-bridge-${i}`}>skep{i} bridge</Label>
                  <Select
                    id={`ed-bridge-${i}`}
                    value={n.bridge}
                    onChange={(e) => setNics(nics.map((x, j) => (j === i ? { ...x, bridge: e.target.value } : x)))}
                    disabled={busy}
                  >
                    {!bridges.includes(n.bridge) ? <option value={n.bridge}>{n.bridge}</option> : null}
                    {bridges.map((b) => (
                      <option key={b} value={b}>
                        {b}
                      </option>
                    ))}
                  </Select>
                </div>
                <FieldText>{n.mode === "dhcp" ? "DHCP" : n.address}, fixed</FieldText>
              </div>
            ))
          )}
        </fieldset>

        {nsAllowed ? (
          <div className="space-y-1.5">
            <Label htmlFor="ed-ns">Nameservers</Label>
            <Input id="ed-ns" value={nameservers} onChange={(e) => setNameservers(e.target.value)} disabled={busy} />
            <FieldText error={Boolean(problems.nameservers)}>{problems.nameservers ?? "Up to three; empty removes them."}</FieldText>
          </div>
        ) : null}

        {withSSH ? (
          <div className="space-y-1.5">
            <Label htmlFor="ed-ssh">SSH key for root</Label>
            <textarea
              id="ed-ssh"
              rows={2}
              className="w-full rounded-md border bg-background px-3 py-2 font-mono text-xs"
              value={sshKey}
              onChange={(e) => setSshKey(e.target.value)}
              disabled={busy}
            />
            <FieldText error={Boolean(problems.sshKey)}>{problems.sshKey ?? "One OpenSSH public key line; empty removes the key."}</FieldText>
          </div>
        ) : fresh && tpl.data && !tpl.data.has_sshd ? (
          <FieldText data-testid="no-sshd">The template has no sshd, so no SSH key can be set.</FieldText>
        ) : null}

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Options</legend>
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={Boolean(options.sysvipc)} onChange={(e) => setOptions({ ...options, sysvipc: e.target.checked })} disabled={busy} />
            <span>
              System V IPC
              <span className="block text-xs text-muted-foreground">Its own shared memory, semaphores, and message queues.</span>
            </span>
          </label>
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={Boolean(options.mlock)} onChange={(e) => setOptions({ ...options, mlock: e.target.checked })} disabled={busy} />
            <span>
              Lock memory
              <span className="block text-xs text-muted-foreground">Lets the jail use mlock(2). Needs a locked-memory limit, and RACCT.</span>
            </span>
          </label>
          {problems.mlock ? <FieldText error>{problems.mlock}</FieldText> : null}
        </fieldset>

        <LimitsFields form={limitsForm} onChange={setLimitsForm} problems={limitProblems} caps={caps} disabled={busy} />
        <FieldText>The limits replace the current ones: an emptied field removes that limit. RCTL limits apply from the next start, the disk quota at once; a quota below what the jail already references is refused.</FieldText>

        {error ? <InlineError error={error} /> : null}
        <DialogActions>
          <Button type="button" variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" disabled={!ready || busy}>
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogActions>
      </form>
    </Dialog>
  );
}
