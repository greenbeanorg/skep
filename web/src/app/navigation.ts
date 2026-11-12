import type { ComponentType } from "react";

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
  /** Set for sections that are not built yet: what it will hold, and when. */
  planned?: { summary: string; milestone: string };
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

// The sections are the ones listed in CLAUDE.md, plus Tasks, which the API
// already serves. Sections without a backend are honest placeholders.
export const NAV: NavGroup[] = [
  {
    label: "Overview",
    items: [
      { path: "/", label: "Dashboard", icon: DashboardIcon },
      {
        path: "/cluster",
        label: "Cluster",
        icon: ClusterIcon,
        planned: {
          summary: "Cluster membership, quorum, and node roles.",
          milestone: "Milestone 8, after the cluster technology is decided",
        },
      },
      { path: "/nodes", label: "Nodes", icon: NodeIcon },
      { path: "/tasks", label: "Tasks", icon: TasksIcon },
    ],
  },
  {
    label: "Workloads",
    items: [
      {
        path: "/vms",
        label: "Virtual machines",
        icon: VmIcon,
        planned: { summary: "bhyve virtual machines: lifecycle, disks, console.", milestone: "Milestone 6" },
      },
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
      {
        path: "/storage",
        label: "Storage",
        icon: StorageIcon,
        planned: {
          summary:
            "OpenZFS pools, datasets, volumes, and snapshots. The pools discovered on this node are already listed under Nodes.",
          milestone: "Milestone 7",
        },
      },
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
      {
        path: "/monitoring",
        label: "Monitoring",
        icon: MonitoringIcon,
        planned: { summary: "Metrics and resource history.", milestone: "A later milestone" },
      },
    ],
  },
  {
    label: "Administration",
    items: [
      {
        path: "/users",
        label: "Users and permissions",
        icon: UsersIcon,
        planned: { summary: "Users, roles, and sessions.", milestone: "Milestone 4" },
      },
      {
        path: "/settings",
        label: "Settings",
        icon: SettingsIcon,
        planned: { summary: "Node and interface settings.", milestone: "A later milestone" },
      },
    ],
  },
];

export const NAV_ITEMS: NavItem[] = NAV.flatMap((g) => g.items);
