import { describe, expect, it } from "vitest";

import {
  EMPTY_LIMITS,
  RACCT_OFF_FIELD,
  cloneBlocked,
  containerErrorHint,
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
  sizeText,
  limitsToForm,
  nicToForm,
  startedNICSpecs,
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

describe("edit helpers", () => {
  it("shows sizes and counts as the form takes them", () => {
    expect(sizeText(2 * 2 ** 30)).toBe("2G");
    expect(sizeText(512 << 20)).toBe("512M");
    expect(sizeText(1000)).toBe("1000");
    expect(limitsToForm({ memory: 512 << 20, open_files: 1024, disk_quota: 1 << 30 })).toEqual({
      ...EMPTY_LIMITS,
      memory: "512M",
      open_files: "1024",
      disk_quota: "1G",
    });
    // What the form shows parses back to the same limits.
    const l = { memory: 512 << 20, locked_memory: 64 << 20, cpu_percent: 50, max_processes: 40, open_files: 1024, disk_quota: 1 << 30 };
    expect(parseLimits(limitsToForm(l), caps).limits).toEqual(l);
  });

  it("keeps a started jail's addressing and changes only the bridge", () => {
    const c = jail({ started_once: true, nics: [{ ...staticNIC, ipv4: { mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" } }] });
    expect(startedNICSpecs(c, ["bridge1"])).toEqual([
      { bridge: "bridge1", ipv4: { mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" } },
    ]);
    expect(nicToForm(c.nics[0]!)).toEqual({ bridge: "bridge0", mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" });
  });
});

describe("containerErrorHint for the security review codes", () => {
  it("words the disk quota refusal for a create and for an existing jail", () => {
    const create = containerErrorHint("container_disk_quota_below_usage", "create");
    const existing = containerErrorHint("container_disk_quota_below_usage", "existing");
    expect(create).toMatch(/template's size/);
    expect(create).not.toMatch(/remove data inside the jail/);
    expect(existing).toMatch(/already holds/);
    expect(existing).not.toMatch(/new jail|template/);
    expect(containerErrorHint("container_disk_quota_below_usage")).toBe(existing);
  });
  it("explains nameservers without a NIC", () => {
    expect(containerErrorHint("nameservers_require_nic")).toMatch(/network interface/);
  });
  it("explains the MAC refusals of amendment n", () => {
    expect(containerErrorHint("container_mac_invalid")).toMatch(/locally administered/);
    expect(containerErrorHint("container_mac_in_use")).toMatch(/already on the bridge/);
  });
  it("explains a quarantined jail", () => {
    expect(containerErrorHint("container_quarantined")).toMatch(/quarantined/);
    expect(containerErrorHint("container_remove_failed")).toMatch(/jail -R/);
  });
  it("explains the locked-memory total of amendment o", () => {
    expect(containerErrorHint("container_locked_memory_above_max")).toMatch(/max_locked_memory_bytes/);
    expect(containerErrorHint("container_locked_memory_exhausted")).toMatch(/Stop a jail with locked memory/);
  });
  it("explains a disk quota below usage (amendment w)", () => {
    expect(containerErrorHint("container_disk_quota_below_usage")).toMatch(/below what the jail's root already holds/);
  });
});

describe("kernel module hints", () => {
  // The hints append to kld_list: a kld_list="..." line pasted into
  // rc.conf would replace the host's existing list.
  it("give the appending sysrc form", () => {
    expect(containerErrorHint("epair_unavailable")).toContain('sysrc kld_list+="if_epair"');
    expect(containerErrorHint("port_filter_unavailable")).toContain('sysrc kld_list+="netgraph ng_socket ng_ether ng_bpf"');
    for (const code of ["epair_unavailable", "port_filter_unavailable"]) {
      expect(containerErrorHint(code)).not.toContain('kld_list="');
    }
  });
});
