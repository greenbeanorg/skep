import { describe, expect, it } from "vitest";

import {
  EMPTY_LIMITS,
  RACCT_OFF_FIELD,
  cloneBlocked,
  mlockProblem,
  nameserversAllowed,
  newNIC,
  nicProblem,
  nicsProblem,
  parseCIDR,
  parseLimits,
  parseNameservers,
  sshKeyProblem,
  staticSlots,
  toNICSpec,
} from "./container";
import { jail, staticNIC } from "./fixtures";

const caps = { rctl: true, max_cpu_percent: 400 };

describe("parseLimits", () => {
  it("leaves empty fields out", () => {
    expect(parseLimits(EMPTY_LIMITS, caps)).toEqual({ limits: {}, problems: {} });
  });

  it("parses sizes and counts", () => {
    const r = parseLimits(
      { memory: "512M", locked_memory: "64M", cpu_percent: "150", max_processes: "40", open_files: "1024", disk_quota: "1G" },
      caps,
    );
    expect(r.problems).toEqual({});
    expect(r.limits).toEqual({
      memory: 512 << 20,
      locked_memory: 64 << 20,
      cpu_percent: 150,
      max_processes: 40,
      open_files: 1024,
      disk_quota: 1 << 30,
    });
  });

  it("enforces the API's minimums and bounds", () => {
    const r = parseLimits(
      { memory: "8M", locked_memory: "x", cpu_percent: "401", max_processes: "0", open_files: "63", disk_quota: "32M" },
      caps,
    );
    expect(Object.keys(r.problems).sort()).toEqual(
      ["cpu_percent", "disk_quota", "locked_memory", "max_processes", "memory", "open_files"].sort(),
    );
    expect(r.problems.cpu_percent).toMatch(/At most 400/);
  });

  it("refuses locked memory above memory", () => {
    const r = parseLimits({ ...EMPTY_LIMITS, memory: "64M", locked_memory: "128M" }, caps);
    expect(r.problems.locked_memory).toMatch(/cannot be more than the memory limit/);
  });

  it("marks every RCTL limit when RACCT is off, but not the disk quota", () => {
    const r = parseLimits({ ...EMPTY_LIMITS, memory: "64M", open_files: "128", disk_quota: "1G" }, { rctl: false, max_cpu_percent: 400 });
    expect(r.problems).toEqual({ memory: RACCT_OFF_FIELD, open_files: RACCT_OFF_FIELD });
    expect(r.limits.disk_quota).toBe(1 << 30);
  });
});

describe("mlockProblem", () => {
  it("needs a locked-memory limit and RACCT", () => {
    expect(mlockProblem(false, {}, caps)).toBeUndefined();
    expect(mlockProblem(true, {}, caps)).toMatch(/locked-memory limit/);
    expect(mlockProblem(true, { memory: 1 << 30 }, caps)).toMatch(/locked-memory limit/);
    expect(mlockProblem(true, { locked_memory: 1 << 20 }, caps)).toBeUndefined();
    expect(mlockProblem(true, { locked_memory: 1 << 20 }, { rctl: false, max_cpu_percent: 400 })).toMatch(/RACCT/);
  });
});

describe("NIC addressing", () => {
  it("parses CIDR", () => {
    expect(parseCIDR("192.0.2.10/24")).toEqual({ addr: 0xc000020a, len: 24 });
    expect(parseCIDR("192.0.2.10")).toBeUndefined();
    expect(parseCIDR("192.0.2.256/24")).toBeUndefined();
    expect(parseCIDR("192.0.2.1/33")).toBeUndefined();
  });

  it("checks static addressing and the gateway", () => {
    expect(nicProblem(newNIC("bridge0"))).toBeUndefined();
    expect(nicProblem(newNIC(""))).toMatch(/bridge/);
    const s = { bridge: "bridge0", mode: "static" as const, address: "192.0.2.10/24", gateway: "" };
    expect(nicProblem(s)).toBeUndefined();
    expect(nicProblem({ ...s, address: "192.0.2.10" })).toMatch(/prefix length/);
    expect(nicProblem({ ...s, gateway: "192.0.2.1" })).toBeUndefined();
    expect(nicProblem({ ...s, gateway: "198.51.100.1" })).toMatch(/in the address's prefix/);
    expect(nicProblem({ ...s, gateway: "192.0.2.10" })).toMatch(/another address/);
    expect(nicProblem({ ...s, gateway: "nope" })).toMatch(/IPv4/);
  });

  it("allows one gateway per jail", () => {
    const g = { bridge: "bridge0", mode: "static" as const, address: "192.0.2.10/24", gateway: "192.0.2.1" };
    expect(nicsProblem([g])).toBeUndefined();
    expect(nicsProblem([g, { ...g, address: "192.0.2.11/24" }])).toMatch(/one default route/);
  });

  it("builds the request's NIC", () => {
    expect(toNICSpec(newNIC("bridge0"))).toEqual({ bridge: "bridge0", ipv4: { mode: "dhcp" } });
    expect(toNICSpec({ bridge: "bridge0", mode: "static", address: " 192.0.2.10/24 ", gateway: "" })).toEqual({
      bridge: "bridge0",
      ipv4: { mode: "static", address: "192.0.2.10/24" },
    });
  });

  it("allows nameservers only without DHCP", () => {
    const s = { bridge: "b", mode: "static" as const, address: "192.0.2.10/24", gateway: "" };
    expect(nameserversAllowed([])).toBe(false);
    expect(nameserversAllowed([s])).toBe(true);
    expect(nameserversAllowed([s, newNIC("b")])).toBe(false);
  });

  it("parses nameservers", () => {
    expect(parseNameservers("192.0.2.53, 2001:db8::53")).toEqual({ list: ["192.0.2.53", "2001:db8::53"] });
    expect(parseNameservers("a b c d").problem).toMatch(/At most 3/);
    expect(parseNameservers("dns.example").problem).toMatch(/dns.example/);
  });
});

describe("sshKeyProblem", () => {
  it("accepts one key line and refuses the rest", () => {
    expect(sshKeyProblem("")).toBeUndefined();
    expect(sshKeyProblem("ssh-ed25519 AAAAC3Nza andy@merrick")).toBeUndefined();
    expect(sshKeyProblem("ssh-ed25519 AAAA\nssh-rsa BBBB")).toMatch(/one line/);
    expect(sshKeyProblem('command="x" ssh-ed25519 AAAA')).toMatch(/without options/);
  });
});

describe("clone rules", () => {
  it("lists static slots", () => {
    expect(staticSlots(jail({ nics: [staticNIC] }))).toEqual([0]);
  });

  it("refuses a started jail with a static address, and explains why", () => {
    expect(cloneBlocked(jail({ nics: [staticNIC] }))).toBeUndefined();
    expect(cloneBlocked(jail({ nics: [staticNIC], started_once: true }))).toMatch(/before any of its code has run/);
    const dhcp = { ...staticNIC, ipv4: { mode: "dhcp" as const } };
    expect(cloneBlocked(jail({ nics: [dhcp], started_once: true }))).toBeUndefined();
    expect(cloneBlocked(jail({ provisioned: false }))).toMatch(/not provisioned/);
  });
});
