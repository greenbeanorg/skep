import { Button } from "@/components/ui/button";
import { FieldText, Input, Label, Select } from "@/components/ui/input";

import { nicProblem, type NICForm } from "./container";

/**
 * One NIC of a jail: the bridge its host end joins, and DHCP or a static
 * IPv4 address with an optional gateway. Inside the jail it is named
 * skep<slot> (amendment k).
 */
export function NICFields({
  slot,
  nic,
  bridges,
  onChange,
  onRemove,
  disabled,
  fixedBridge,
}: {
  slot: number;
  nic: NICForm;
  bridges: string[];
  onChange: (n: NICForm) => void;
  onRemove?: () => void;
  disabled?: boolean;
  /** For a clone: the bridge stays the source's. */
  fixedBridge?: boolean;
}) {
  const problem = nicProblem(nic);
  const id = (k: string) => `nic-${slot}-${k}`;
  return (
    <div className="space-y-2 rounded-md border p-3" data-testid={`nic-${slot}`}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">
          skep{slot}
          <span className="font-normal text-muted-foreground"> inside the jail</span>
        </span>
        {onRemove ? (
          <Button type="button" size="sm" variant="ghost" onClick={onRemove} disabled={disabled}>
            Remove
          </Button>
        ) : null}
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        <div className="space-y-1">
          <Label htmlFor={id("bridge")}>Bridge</Label>
          <Select
            id={id("bridge")}
            value={nic.bridge}
            onChange={(e) => onChange({ ...nic, bridge: e.target.value })}
            disabled={disabled || fixedBridge}
          >
            {nic.bridge !== "" && !bridges.includes(nic.bridge) ? <option value={nic.bridge}>{nic.bridge}</option> : null}
            {bridges.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor={id("mode")}>IPv4</Label>
          <Select
            id={id("mode")}
            value={nic.mode}
            onChange={(e) => onChange({ ...nic, mode: e.target.value as NICForm["mode"] })}
            disabled={disabled}
          >
            <option value="dhcp">DHCP</option>
            <option value="static">Static</option>
          </Select>
        </div>
      </div>
      {nic.mode === "static" ? (
        <div className="grid gap-2 sm:grid-cols-2">
          <div className="space-y-1">
            <Label htmlFor={id("address")}>Address</Label>
            <Input
              id={id("address")}
              value={nic.address}
              placeholder="192.0.2.10/24"
              onChange={(e) => onChange({ ...nic, address: e.target.value })}
              disabled={disabled}
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor={id("gateway")}>Gateway (optional)</Label>
            <Input
              id={id("gateway")}
              value={nic.gateway}
              placeholder="192.0.2.1"
              onChange={(e) => onChange({ ...nic, gateway: e.target.value })}
              disabled={disabled}
            />
          </div>
        </div>
      ) : null}
      {problem ? <FieldText error>{problem}</FieldText> : null}
    </div>
  );
}
