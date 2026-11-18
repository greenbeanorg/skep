import { useState, type FormEvent } from "react";

import { api, toApiError, type ApiError } from "@/api/client";
import { useQuery } from "@/api/hooks";
import { PERMISSIONS, type ContainerCreate, type ContainerCreated, type ContainerOptions } from "@/api/types";
import { useSession } from "@/app/session";
import { useSystem } from "@/app/system";
import { InlineError } from "@/components/errors";
import { Button } from "@/components/ui/button";
import { Dialog, DialogActions } from "@/components/ui/dialog";
import { FieldText, Input, Label, Select } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";

import {
  CONTAINER_NAME_RE,
  EMPTY_LIMITS,
  MAX_NICS,
  NAME_HELP,
  containerErrorHint,
  mlockProblem,
  nameserversAllowed,
  newNIC,
  nicProblem,
  nicsProblem,
  parseLimits,
  parseNameservers,
  sshKeyProblem,
  toNICSpec,
  type LimitsForm,
  type NICForm,
} from "./container";
import { LimitsFields } from "./LimitsFields";
import { NICFields } from "./NICFields";

export function withHint(err: ApiError): ApiError {
  const hint = containerErrorHint(err.code);
  return hint ? Object.assign(err, { message: `${err.message} ${hint}` }) : err;
}

/**
 * The create form: a template, the name, NICs, an SSH key, the two
 * options, and the limits (ADR 0010 decisions 9 to 12). The API checks
 * every rule again.
 */
export function CreateContainerDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (c: ContainerCreated) => void }) {
  const { can } = useSession();
  const system = useSystem();
  const mayListTemplates = can(PERMISSIONS.storageRead);
  const templates = useQuery(mayListTemplates ? "storage:templates" : null, (o) => api.templates(o));
  const bridges = system.data?.bhyve?.bridges ?? [];
  const caps = system.data?.containers;

  const [templateId, setTemplateId] = useState("");
  const [name, setName] = useState("");
  const [hostname, setHostname] = useState("");
  const [nics, setNics] = useState<NICForm[]>([]);
  const [nameservers, setNameservers] = useState("");
  const [sshKey, setSshKey] = useState("");
  const [options, setOptions] = useState<ContainerOptions>({});
  const [limitsForm, setLimitsForm] = useState<LimitsForm>(EMPTY_LIMITS);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | undefined>();

  const usable = (templates.data?.items ?? []).filter((t) => t.provisioned);
  const chosen = usable.find((t) => t.id === templateId) ?? usable[0];
  const { limits, problems: limitProblems } = parseLimits(limitsForm, caps);
  const nsAllowed = nameserversAllowed(nics);
  const ns = parseNameservers(nameservers);
  const problems = {
    name: name === "" || CONTAINER_NAME_RE.test(name) ? undefined : NAME_HELP,
    hostname: hostname.length > 253 ? "At most 253 characters." : undefined,
    nics: nicsProblem(nics) ?? (nics.some((n) => nicProblem(n)) ? "Fix the network interfaces above." : undefined),
    nameservers: nsAllowed ? ns.problem : undefined,
    sshKey: sshKeyProblem(sshKey),
    mlock: mlockProblem(Boolean(options.mlock), limits, caps),
  };
  const ready =
    chosen !== undefined && name !== "" && !Object.values(problems).some(Boolean) && !Object.values(limitProblems).some(Boolean);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || busy || !chosen) return;
    const req: ContainerCreate = {
      name,
      template_id: chosen.id,
      ...(hostname.trim() ? { hostname: hostname.trim() } : {}),
      ...(options.sysvipc || options.mlock ? { options } : {}),
      ...(Object.keys(limits).length ? { limits } : {}),
      ...(sshKey.trim() ? { ssh_authorized_key: sshKey.trim() } : {}),
      ...(nics.length ? { nics: nics.map(toNICSpec) } : {}),
      ...(nsAllowed && ns.list.length ? { nameservers: ns.list } : {}),
    };
    setBusy(true);
    setError(undefined);
    try {
      onCreated(await api.createContainer(req));
    } catch (err) {
      setError(withHint(toApiError(err)));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      className="max-w-2xl"
      title="Create a container"
      description="A FreeBSD jail with its own network stack, cloned from a template. Nothing in the form becomes a command on the host."
    >
      <form onSubmit={submit} className="space-y-4" noValidate>
        {!mayListTemplates ? (
          <Notice tone="warn" title="Templates cannot be listed">
            Your role lacks storage:read, which listing the templates needs, so no jail can be created from here.
          </Notice>
        ) : templates.data && usable.length === 0 ? (
          <Notice tone="warn" title="No template is built">
            A jail is cloned from a built template. An administrator with storage:write builds one through the API (POST /storage/templates).
          </Notice>
        ) : null}
        {templates.error ? <InlineError error={templates.error} /> : null}

        <div className="space-y-1.5">
          <Label htmlFor="ct-template">Template</Label>
          <Select id="ct-template" value={chosen?.id ?? ""} onChange={(e) => setTemplateId(e.target.value)} disabled={busy || usable.length === 0}>
            {usable.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} ({t.release ?? t.source}, {t.pool})
              </option>
            ))}
          </Select>
          {chosen ? (
            <FieldText>
              {chosen.source === "dist" ? "From base.txz." : `Packages: ${chosen.packages.join(", ") || "none listed"}.`}
              {chosen.patch_differs ? " Its patch level differs from the host's." : ""}
            </FieldText>
          ) : null}
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="ct-name">Name</Label>
            <Input id="ct-name" value={name} onChange={(e) => setName(e.target.value)} required autoFocus disabled={busy} />
            {problems.name ? <FieldText error>{problems.name}</FieldText> : null}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ct-hostname">Hostname</Label>
            <Input id="ct-hostname" value={hostname} placeholder={name || "the name"} onChange={(e) => setHostname(e.target.value)} disabled={busy} />
            <FieldText error={Boolean(problems.hostname)}>{problems.hostname ?? "Defaults to the name."}</FieldText>
          </div>
        </div>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Network</legend>
          {nics.length === 0 ? <p className="text-sm text-muted-foreground">No network interface: the jail has only its loopback.</p> : null}
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
          {problems.nics && nicsProblem(nics) ? <FieldText error>{problems.nics}</FieldText> : null}
          {bridges.length === 0 ? (
            <FieldText>The agent's network.bridges lists no bridge, so the jail can have no network interface.</FieldText>
          ) : nics.length < MAX_NICS ? (
            <Button type="button" size="sm" variant="outline" onClick={() => setNics([...nics, newNIC(bridges[0] ?? "")])} disabled={busy}>
              Add network interface
            </Button>
          ) : null}
          <FieldText>
            Each interface is an epair: the host end joins the bridge, the jail end is skep0, skep1, … inside the jail. The addressing
            is written into the jail's rc.conf before its first start; afterwards it is the jail's own business.
          </FieldText>
        </fieldset>

        {nsAllowed ? (
          <div className="space-y-1.5">
            <Label htmlFor="ct-ns">Nameservers</Label>
            <Input id="ct-ns" value={nameservers} placeholder="192.0.2.53" onChange={(e) => setNameservers(e.target.value)} disabled={busy} />
            <FieldText error={Boolean(problems.nameservers)}>
              {problems.nameservers ?? "Up to three, separated by spaces or commas, for the jail's resolv.conf."}
            </FieldText>
          </div>
        ) : nics.length > 0 ? (
          <FieldText>With a DHCP interface, dhclient writes the jail's resolv.conf, so no nameservers are given here.</FieldText>
        ) : null}

        <div className="space-y-1.5">
          <Label htmlFor="ct-ssh">SSH key for root (optional)</Label>
          <textarea
            id="ct-ssh"
            rows={2}
            className="w-full rounded-md border bg-background px-3 py-2 font-mono text-xs"
            value={sshKey}
            placeholder="ssh-ed25519 AAAA… you@host"
            onChange={(e) => setSshKey(e.target.value)}
            disabled={busy}
          />
          <FieldText error={Boolean(problems.sshKey)} data-testid="ssh-help">
            {problems.sshKey ??
              "One OpenSSH public key line. Skep turns sshd on with root login by key only. It needs a template with sshd (built with FreeBSD-ssh); the API refuses a key for a template without it."}
          </FieldText>
        </div>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Options</legend>
          <label className="flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={Boolean(options.sysvipc)}
              onChange={(e) => setOptions({ ...options, sysvipc: e.target.checked })}
              disabled={busy}
            />
            <span>
              System V IPC
              <span className="block text-xs text-muted-foreground">Its own shared memory, semaphores, and message queues.</span>
            </span>
          </label>
          <label className="flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={Boolean(options.mlock)}
              onChange={(e) => setOptions({ ...options, mlock: e.target.checked })}
              disabled={busy}
            />
            <span>
              Lock memory
              <span className="block text-xs text-muted-foreground">
                Lets the jail use mlock(2). Needs a locked-memory limit below, which caps what it may lock, and RACCT.
              </span>
            </span>
          </label>
          {problems.mlock ? <FieldText error>{problems.mlock}</FieldText> : null}
        </fieldset>

        <LimitsFields form={limitsForm} onChange={setLimitsForm} problems={limitProblems} caps={caps} disabled={busy} />

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
