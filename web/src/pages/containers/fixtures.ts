// Test data of the container pages, shared by their tests.

import type { Container, ContainerNIC, Template } from "@/api/types";

export function jail(extra: Partial<Container> = {}): Container {
  return {
    id: "6c1f3c43-6a51-4bc0-9f6e-1a7f0c1d2e3f",
    name: "web1",
    hostname: "web1",
    template_id: "t1",
    template_name: "minimal-151",
    pool: "skeptest",
    options: {},
    limits: {},
    nics: [],
    provisioned: true,
    started_once: false,
    desired_state: "stopped",
    state: "stopped",
    created_at: "2026-11-17T00:00:00Z",
    updated_at: "2026-11-17T00:00:00Z",
    ...extra,
  };
}

export const staticNIC: ContainerNIC = {
  id: "n0",
  slot: 0,
  bridge: "bridge0",
  mac_address: "02:00:00:00:00:01",
  interface: "skep0",
  ipv4: { mode: "static", address: "192.0.2.10/24" },
};

export const dhcpNIC: ContainerNIC = { ...staticNIC, id: "n1", ipv4: { mode: "dhcp" } };

export function template(extra: Partial<Template> = {}): Template {
  return {
    id: "t1",
    name: "minimal-151",
    pool: "skeptest",
    source: "pkgbase",
    packages: ["FreeBSD-runtime", "FreeBSD-ssh"],
    provisioned: true,
    release: "15.1-RELEASE-p6",
    patch_differs: false,
    used_bytes: 78 << 20,
    created_at: "2026-11-16T00:00:00Z",
    ...extra,
  };
}
