import { describe, expect, it } from "vitest";

import { consoleURL } from "@/api/client";

import { VM_NAME_RE, parseSize, sizeProblem, vmErrorHint } from "./vm";

describe("parseSize", () => {
  it("reads bytes and binary suffixes as the API does", () => {
    expect(parseSize("1048576")).toBe(1 << 20);
    expect(parseSize("512M")).toBe(512 * 2 ** 20);
    expect(parseSize(" 8g ")).toBe(8 * 2 ** 30);
    expect(parseSize("1T")).toBe(2 ** 40);
  });
  it("refuses anything else", () => {
    for (const s of ["", "8 GB", "1.5G", "-1G", "8K", "G"]) expect(parseSize(s)).toBeUndefined();
  });
});

describe("sizeProblem", () => {
  it("applies the minimum and the MiB rule", () => {
    expect(sizeProblem(parseSize("64M"), 128 << 20, "memory")).toMatch(/at least 128 MiB/);
    expect(sizeProblem(parseSize("1048577"), 1 << 20, "disk size")).toMatch(/whole number of MiB/);
    expect(sizeProblem(undefined, 1, "memory")).toMatch(/such as 8G/);
    expect(sizeProblem(parseSize("1G"), 128 << 20, "memory")).toBeUndefined();
  });
});

describe("VM_NAME_RE", () => {
  it("matches the API's rule", () => {
    expect(VM_NAME_RE.test("web-1.lab_a")).toBe(true);
    for (const s of ["-x", "a b", "x".repeat(64), ""]) expect(VM_NAME_RE.test(s)).toBe(false);
  });
});

describe("consoleURL", () => {
  it("uses wss on an https origin and escapes the ID", () => {
    expect(consoleURL("a/b", { protocol: "https:", host: "h:8443" })).toBe("wss://h:8443/api/v1/vms/a%2Fb/console/serial");
    expect(consoleURL("x", { protocol: "http:", host: "h" })).toBe("ws://h/api/v1/vms/x/console/serial");
  });
});

describe("vmErrorHint", () => {
  it("gives the appending sysrc form for the netgraph modules", () => {
    const hint = vmErrorHint("port_filter_unavailable");
    expect(hint).toContain('sysrc kld_list+="netgraph ng_socket ng_ether ng_bpf"');
    expect(hint).not.toContain('kld_list="');
  });
});
