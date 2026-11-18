import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Container, Snapshot } from "@/api/types";

import { ContainerDetailPage, powerActions } from "./ContainerDetailPage";
import { dhcpNIC, jail, staticNIC } from "./fixtures";
import { fakeApi, renderAt, type Reply } from "./testing";

const ID = jail().id;
const BASE = `/api/v1/containers/${ID}`;
const VIEWER = ["system:read", "vm:read", "storage:read", "container:read", "task:read"];
const OPERATOR = [...VIEWER, "container:write", "container:delete", "container:power", "container:snapshot"];

function snap(id: string, name: string, extra: Partial<Snapshot> = {}): Snapshot {
  return {
    id,
    resource_kind: "container",
    resource_id: ID,
    name,
    description: "",
    consistency: "consistent",
    provisioned: true,
    created_at: "2026-11-17T01:00:00Z",
    clones: [],
    ...extra,
  };
}

function replies(c: Container, extra: Record<string, Reply> = {}): Record<string, Reply> {
  return {
    [`GET ${BASE}`]: { status: 200, body: c },
    [`GET ${BASE}/snapshots`]: { status: 200, body: { items: [] } },
    [`GET ${BASE}/console-log`]: { status: 200, body: { text: "", truncated: false } },
    "GET /api/v1/metrics": {
      status: 200,
      body: {
        node_id: "n1",
        observed_at: "2026-11-17T02:00:00Z",
        capacity_warn_percent: 80,
        capacity_fail_percent: 90,
        pools: [],
        vms: [],
        containers: [
          {
            id: ID,
            name: c.name,
            state: c.state,
            cpu_percent: 12,
            cpu_time_seconds: 75,
            resident_memory_bytes: 96 << 20,
            processes: 9,
            open_file_slots: 420,
            disk_used_bytes: 4 << 20,
            disk_referenced_bytes: 82 << 20,
          },
        ],
      },
    },
    ...extra,
  };
}

function renderPage() {
  renderAt(<ContainerDetailPage />, `/containers/${ID}`, "/containers/:id");
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("powerActions", () => {
  it("follows the state", () => {
    expect(powerActions(jail({ state: "stopped" }))).toEqual(["start"]);
    expect(powerActions(jail({ state: "running" }))).toEqual(["stop", "restart"]);
    expect(powerActions(jail({ state: "starting" }))).toEqual([]);
    expect(powerActions(jail({ provisioned: false }))).toEqual([]);
  });

  it("offers nothing for a quarantined jail", () => {
    expect(powerActions(jail({ state: "failed" }))).toEqual([]);
  });
});

describe("ContainerDetailPage quarantine", () => {
  it("explains a quarantined jail, offers no start or stop, and allows delete", async () => {
    fakeApi(OPERATOR, replies(jail({ state: "failed", state_detail: "the last start failed: container_quarantined: ..." })));
    renderPage();
    expect(await screen.findByTestId("quarantine-note")).toHaveTextContent(/without running any of its code/);
    for (const name of ["Start", "Stop", "Restart"]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Edit" })).toBeDisabled();
  });
});

describe("ContainerDetailPage permissions", () => {
  it("shows a viewer no actions", async () => {
    fakeApi(VIEWER, replies(jail()));
    renderPage();
    await screen.findByRole("heading", { name: "web1" });
    for (const name of ["Start", "Delete", "Take snapshot", "Clone"]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
  });

  it("shows an operator every action the state allows", async () => {
    fakeApi(OPERATOR, replies(jail()));
    renderPage();
    expect(await screen.findByRole("button", { name: "Start" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Take snapshot" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Clone" })).toBeEnabled();
  });

  it("needs both container:write and container:snapshot to clone", async () => {
    fakeApi([...VIEWER, "container:snapshot"], replies(jail()));
    renderPage();
    expect(await screen.findByRole("button", { name: "Take snapshot" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clone" })).not.toBeInTheDocument();
  });

  it("needs both container:write and container:snapshot to roll back", async () => {
    const snaps = { [`GET ${BASE}/snapshots`]: { status: 200, body: { items: [snap("s1", "first")] } } };
    fakeApi([...VIEWER, "container:snapshot"], replies(jail(), snaps));
    renderPage();
    expect(await screen.findByText("first")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Take snapshot" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Roll back" })).not.toBeInTheDocument();
  });

  it("offers roll back with container:write and container:snapshot", async () => {
    const snaps = { [`GET ${BASE}/snapshots`]: { status: 200, body: { items: [snap("s1", "first")] } } };
    fakeApi([...VIEWER, "container:write", "container:snapshot"], replies(jail(), snaps));
    renderPage();
    expect(await screen.findByRole("button", { name: "Roll back" })).toBeEnabled();
  });

  it("disables delete while the jail runs", async () => {
    fakeApi(OPERATOR, replies(jail({ state: "running", jid: 7 })));
    renderPage();
    expect(await screen.findByRole("button", { name: "Stop" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled();
  });
});

describe("ContainerDetailPage clone rules", () => {
  it("explains why a started jail with a static address cannot be cloned", async () => {
    fakeApi(OPERATOR, replies(jail({ started_once: true, nics: [staticNIC] })));
    renderPage();
    expect(await screen.findByTestId("clone-blocked")).toHaveTextContent(/before any of its code has run/);
    expect(screen.getByRole("button", { name: "Clone" })).toBeDisabled();
  });

  it("asks a never-started static jail's clone for new addressing", async () => {
    const created = { container: jail({ id: "c2", name: "web2" }), task: { id: "t2", type: "container.clone", status: "pending" } };
    const calls = fakeApi(
      OPERATOR,
      replies(jail({ nics: [staticNIC, { ...dhcpNIC, slot: 1, interface: "skep1" }], nameservers: ["192.0.2.53"] }), {
        [`POST ${BASE}/clone`]: { status: 202, body: created },
      }),
    );
    renderPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Clone" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByTestId("clone-identity-note")).toHaveTextContent(/SSH host keys/);
    expect(within(dialog).getByTestId("clone-addressing")).toHaveTextContent(/skep0/);
    // Not every NIC is static here, so the source's nameservers are dropped and not offered.
    expect(within(dialog).queryByLabelText("Nameservers")).not.toBeInTheDocument();
    await user.type(within(dialog).getByLabelText("Name of the new jail"), "web2");
    expect(within(dialog).getByRole("button", { name: "Clone" })).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Address"), "192.0.2.11/24");
    await user.click(within(dialog).getByRole("button", { name: "Clone" }));
    await waitFor(() => expect(calls.some((c) => c.key === `POST ${BASE}/clone`)).toBe(true));
    expect(calls.find((c) => c.key === `POST ${BASE}/clone`)?.body).toEqual({
      name: "web2",
      nics: [{ bridge: "bridge0", ipv4: { mode: "static", address: "192.0.2.11/24" } }, { bridge: "bridge0" }],
    });
  });
});

describe("ContainerDetailPage clone nameservers", () => {
  it("pre-fills the source's nameservers, marked inherited", async () => {
    fakeApi(OPERATOR, replies(jail({ nics: [staticNIC], nameservers: ["192.0.2.53", "192.0.2.54"] })));
    renderPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Clone" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText("Nameservers")).toHaveValue("192.0.2.53 192.0.2.54");
    expect(within(dialog).getByTestId("clone-ns-note")).toHaveTextContent(/Inherited from source/);
  });
});

describe("ContainerDetailPage snapshots", () => {
  it("needs a confirmation to destroy newer snapshots on rollback", async () => {
    const calls = fakeApi(
      OPERATOR,
      replies(jail(), {
        [`GET ${BASE}/snapshots`]: { status: 200, body: { items: [snap("s1", "first"), snap("s2", "second")] } },
        [`POST ${BASE}/snapshots/s1/rollback`]: { status: 202, body: { id: "t3", type: "container.snapshot.rollback", status: "pending" } },
      }),
    );
    renderPage();
    const user = userEvent.setup();
    await screen.findByText("first");
    await user.click(screen.getAllByRole("button", { name: "Roll back" })[0]!);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/destroys second/);
    const go = within(dialog).getByRole("button", { name: "Roll back" });
    expect(go).toBeDisabled();
    await user.click(within(dialog).getByRole("checkbox"));
    await user.click(go);
    await waitFor(() => expect(calls.some((c) => c.key === `POST ${BASE}/snapshots/s1/rollback`)).toBe(true));
    expect(calls.find((c) => c.key === `POST ${BASE}/snapshots/s1/rollback`)?.body).toEqual({ destroy_newer: true });
  });
});

describe("ContainerDetailPage usage, RACCT, and tasks", () => {
  it("shows usage from /metrics with the memory limit marked soft", async () => {
    fakeApi(OPERATOR, replies(jail({ state: "running", limits: { memory: 64 << 20, open_files: 1024 } })));
    renderPage();
    const usage = await screen.findByTestId("usage");
    await waitFor(() => expect(usage).toHaveTextContent("96.0 MiB"));
    expect(usage).toHaveTextContent(/of 64.0 MiB \(soft\)/);
    expect(usage).toHaveTextContent(/Over the memory limit: it is soft/);
    expect(usage).toHaveTextContent(/420 of 1024/);
  });

  it("does not ask for /metrics without every permission it needs", async () => {
    const calls = fakeApi(["container:read"], replies(jail()));
    renderPage();
    expect(await screen.findByTestId("usage")).toHaveTextContent(/need system:read, vm:read, storage:read, and container:read/);
    expect(calls.some((c) => c.key === "GET /api/v1/metrics")).toBe(false);
  });

  it("warns that a jail with RCTL limits cannot start without RACCT", async () => {
    fakeApi(OPERATOR, replies(jail({ limits: { memory: 64 << 20 } })), false);
    renderPage();
    expect(await screen.findByText("This jail cannot start while RACCT is off")).toBeInTheDocument();
  });

  it("shows a failed start's code, message, and hint", async () => {
    fakeApi(
      OPERATOR,
      replies(jail(), {
        [`POST ${BASE}/actions`]: { status: 202, body: { id: "t4", type: "container.start", status: "pending" } },
        "GET /api/v1/tasks/t4": {
          status: 200,
          body: {
            id: "t4",
            type: "container.start",
            status: "failed",
            error: { code: "container_start_failed", message: "/etc/rc failed" },
            events: [],
          },
        },
        [`GET ${BASE}/console-log`]: { status: 200, body: { text: "Starting jail...\nrc failed\n", truncated: false } },
      }),
    );
    renderPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Start" }));
    const line = await screen.findByTestId("task-line");
    await waitFor(() => expect(line).toHaveTextContent("container_start_failed"));
    expect(line).toHaveTextContent("/etc/rc failed");
    expect(line).toHaveTextContent(/console log below/);
    expect(await screen.findByTestId("console-log")).toHaveTextContent("rc failed");
  });
});
