import type { ComponentType } from "react";

import { PERMISSIONS, type Permission } from "@/api/types";
import {
  BackupIcon,
  ClusterIcon,
  ContainerIcon,
  DashboardIcon,
  HaIcon,
  MonitoringIcon,
  NetworkIcon,
  NodeIcon,
  SettingsIcon,
  StorageIcon,
  TasksIcon,
  UsersIcon,
  VmIcon,
  type IconProps,
} from "@/components/icons";

export interface NavItem {
  path: string;
  label: string;
  icon: ComponentType<IconProps>;
  /**
   * The permission the entry needs. An entry the role lacks is hidden, as
   * a convenience: the server is the authority (ADR 0008, decision 1).
   * Entries without one are shown to everyone logged in.
   */
  permission?: Permission;
  /** Set for sections that are not built yet: what it will hold, and when. */
  planned?: { summary: string; milestone: string };
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

// The twelve sections CLAUDE.md names, with Tasks under Monitoring until
// Monitoring has metrics of its own (ADR 0008, decision 1). Sections
// without a backend are placeholders that show no data.
export const NAV: NavGroup[] = [
  {
    label: "Overview",
    items: [
      { path: "/", label: "Dashboard", icon: DashboardIcon, permission: PERMISSIONS.systemRead },
      {
        path: "/cluster",
        label: "Cluster",
        icon: ClusterIcon,
        planned: {
          summary: "Cluster membership, quorum, and node roles.",
          milestone: "Milestone 8, after the cluster technology is decided",
        },
      },
      { path: "/nodes", label: "Nodes", icon: NodeIcon, permission: PERMISSIONS.nodeRead },
    ],
  },
  {
    label: "Workloads",
    items: [
      { path: "/vms", label: "Virtual machines", icon: VmIcon, permission: PERMISSIONS.vmRead },
      {
        path: "/containers",
        label: "Containers",
        icon: ContainerIcon,
        planned: { summary: "FreeBSD jails: lifecycle, templates, resource limits.", milestone: "Milestone 7" },
      },
    ],
  },
  {
    label: "Infrastructure",
    items: [
      { path: "/storage", label: "Storage", icon: StorageIcon, permission: PERMISSIONS.storageRead },
      {
        path: "/networking",
        label: "Networking",
        icon: NetworkIcon,
        planned: { summary: "Bridges, VLANs, link aggregation, and firewall rules.", milestone: "A later milestone" },
      },
      {
        path: "/ha",
        label: "High availability",
        icon: HaIcon,
        planned: { summary: "Fencing, ownership, and recovery policy.", milestone: "Milestone 9" },
      },
      {
        path: "/backups",
        label: "Backups",
        icon: BackupIcon,
        planned: { summary: "Backup jobs, schedules, and restore.", milestone: "Milestone 9" },
      },
    ],
  },
  {
    label: "Monitoring",
    items: [
      { path: "/tasks", label: "Tasks", icon: TasksIcon, permission: PERMISSIONS.taskRead },
      {
        path: "/monitoring",
        label: "Metrics",
        icon: MonitoringIcon,
        planned: { summary: "Metrics and resource history.", milestone: "A later milestone" },
      },
    ],
  },
  {
    label: "Administration",
    items: [
      { path: "/users", label: "Users and permissions", icon: UsersIcon, permission: PERMISSIONS.userRead },
      { path: "/settings", label: "Settings", icon: SettingsIcon, permission: PERMISSIONS.systemRead },
    ],
  },
];

export const NAV_ITEMS: NavItem[] = NAV.flatMap((g) => g.items);

/** The navigation with the entries a set of permissions does not allow removed. */
export function visibleNav(can: (p: Permission) => boolean): NavGroup[] {
  return NAV.map((g) => ({ ...g, items: g.items.filter((item) => !item.permission || can(item.permission)) })).filter(
    (g) => g.items.length > 0,
  );
}
