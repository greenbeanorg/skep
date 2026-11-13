import { describe, expect, it } from "vitest";

import { PERMISSIONS, type Permission } from "@/api/types";

import { NAV, NAV_ITEMS, visibleNav } from "./navigation";

const labels = (can: (p: Permission) => boolean) => visibleNav(can).flatMap((g) => g.items.map((i) => i.label));

describe("navigation", () => {
  it("names the twelve sections of CLAUDE.md", () => {
    const sections = NAV_ITEMS.map((i) => i.label);
    for (const s of [
      "Dashboard",
      "Cluster",
      "Nodes",
      "Virtual machines",
      "Containers",
      "Storage",
      "Networking",
      "High availability",
      "Backups",
      "Users and permissions",
      "Settings",
    ]) {
      expect(sections).toContain(s);
    }
    expect(NAV.map((g) => g.label)).toContain("Monitoring");
  });

  it("placeholders show no data: they carry a summary and a milestone only", () => {
    for (const item of NAV_ITEMS.filter((i) => i.planned)) {
      expect(item.planned?.summary).toBeTruthy();
      expect(item.planned?.milestone).toBeTruthy();
      expect(item.permission).toBeUndefined();
    }
  });

  it("hides what the role cannot read", () => {
    const admin = labels(() => true);
    expect(admin).toContain("Users and permissions");
    const viewer = labels((p) => p !== PERMISSIONS.userRead && p !== PERMISSIONS.auditRead && p !== PERMISSIONS.userManage);
    expect(viewer).not.toContain("Users and permissions");
    expect(viewer).toContain("Dashboard");
    expect(viewer).toContain("Tasks");
    expect(viewer).toContain("Settings");
    // A group with nothing left disappears.
    const nothing = visibleNav(() => false);
    expect(nothing.map((g) => g.label)).not.toContain("Administration");
    expect(nothing.flatMap((g) => g.items).every((i) => i.planned)).toBe(true);
  });
});
