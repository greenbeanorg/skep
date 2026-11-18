import type { ContainerCapabilities } from "@/api/types";
import { FieldText, Input, Label } from "@/components/ui/input";
import { Notice } from "@/components/ui/notice";

import { RCTL_LIMITS, type LimitsForm } from "./container";

/**
 * What each limit does on FreeBSD 15.1, as measured on the test host (ADR
 * 0010 decision 11, amendment j). Shown under each field, and on the
 * detail page next to the value.
 */
export const LIMIT_HELP: Record<keyof LimitsForm, { label: string; help: string; placeholder: string }> = {
  memory: {
    label: "Memory (soft)",
    placeholder: "none",
    help: "A resident-memory target, not a cap: FreeBSD does not refuse an allocation past it, so a jail can hold more. While it is over, its pages slowly move to the inactive queue and are the first reclaimed when the host runs short. At least 16 MiB.",
  },
  locked_memory: {
    label: "Locked memory",
    placeholder: "none",
    help: "What the jail may lock with mlock(2). The Lock memory option needs it. Not more than the memory limit.",
  },
  cpu_percent: {
    label: "CPU % (approximate)",
    placeholder: "none",
    help: "100 is one CPU. Approximate: RACCT samples and averages, so a busy jail swings above and below the limit around it.",
  },
  max_processes: {
    label: "Processes",
    placeholder: "none",
    help: "The processes in the jail; a fork past it fails.",
  },
  open_files: {
    label: "File-table slots",
    placeholder: "none",
    help: "Counts descriptor-table slots, not open files: each process is charged the size of its table, which grows in steps (40, 128, 256, … slots) and never shrinks, and helper processes such as casper count too. Size it well above the descriptors the jail's programs keep open. At least 64.",
  },
  disk_quota: {
    label: "Disk quota",
    placeholder: "none",
    help: "ZFS refquota on the jail's root; works without RACCT, and snapshots do not count. It includes what the root shares with its template (about 78 MiB for a minimal one), so the room for the jail's own data is the quota less that. At least 64 MiB.",
  },
};

const ORDER: (keyof LimitsForm)[] = ["memory", "locked_memory", "cpu_percent", "max_processes", "open_files", "disk_quota"];

/** The notice for a host without RACCT, from GET /system. */
export function RacctOffNotice({ className }: { className?: string }) {
  return (
    <Notice tone="warn" title="RACCT is off on this host" className={className} data-testid="racct-off">
      kern.racct.enable is 0, so the memory, CPU, process, and file-table limits cannot be enforced: a jail with any of them is
      refused, and so is its start. Only the disk quota works. An administrator enables RACCT in /boot/loader.conf and reboots.
    </Notice>
  );
}

/**
 * The limit fields of the create form. Every field is optional; an empty
 * one means no limit. The RCTL limits are disabled when RACCT is off.
 */
export function LimitsFields({
  form,
  onChange,
  problems,
  caps,
  disabled,
}: {
  form: LimitsForm;
  onChange: (f: LimitsForm) => void;
  problems: Partial<Record<keyof LimitsForm, string>>;
  caps: ContainerCapabilities | undefined;
  disabled?: boolean;
}) {
  const racctOff = caps !== undefined && !caps.rctl;
  return (
    <fieldset className="space-y-3">
      <legend className="text-sm font-medium">Resource limits</legend>
      {racctOff ? <RacctOffNotice /> : null}
      <div className="grid gap-3 sm:grid-cols-2">
        {ORDER.map((key) => {
          const h = LIMIT_HELP[key];
          const rctl = RCTL_LIMITS.includes(key);
          const id = `limit-${key}`;
          const help = key === "cpu_percent" && caps?.max_cpu_percent ? `${h.help} At most ${caps.max_cpu_percent} on this host.` : h.help;
          return (
            <div key={key} className="space-y-1">
              <Label htmlFor={id}>{h.label}</Label>
              <Input
                id={id}
                value={form[key]}
                placeholder={h.placeholder}
                inputMode={key === "memory" || key === "locked_memory" || key === "disk_quota" ? "text" : "numeric"}
                aria-invalid={problems[key] ? true : undefined}
                disabled={disabled || (rctl && racctOff && form[key] === "")}
                onChange={(e) => onChange({ ...form, [key]: e.target.value })}
              />
              <FieldText error={Boolean(problems[key])}>{problems[key] ?? help}</FieldText>
            </div>
          );
        })}
      </div>
    </fieldset>
  );
}
