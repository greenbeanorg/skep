import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Container } from "@/api/types";

import { ContainerDetailPage } from "./ContainerDetailPage";
import { EditContainerDialog, removedLimits } from "./EditContainerDialog";
import { dhcpNIC, jail, staticNIC, template } from "./fixtures";
import { fakeApi, renderAt, type Reply } from "./testing";

const ID = jail().id;
const PUT = `PUT /api/v1/containers/${ID}`;
const ALL = ["*"];
const TASK = { status: 202, body: { id: "t5", type: "container.update", status: "pending" } };

afterEach(() => {
  vi.unstubAllGlobals();
});

function renderEdit(c: Container, replies: Record<string, Reply> = {}, hasSSHD = true) {
  const calls = fakeApi(ALL, {
    "GET /api/v1/storage/templates/t1": { status: 200, body: template({ has_sshd: hasSSHD }) },
    [PUT]: TASK,
    ...replies,
  });
  const onTask = vi.fn();
  renderAt(<EditContainerDialog c={c} onClose={() => {}} onTask={onTask} />);
  return { calls, onTask };
}

describe("EditContainerDialog", () => {
  it("sends hostname, options, and the full limits; leaves unchanged NICs out", async () => {
    const { calls, onTask } = renderEdit(jail({ nics: [dhcpNIC], limits: { memory: 512 << 20, open_files: 1024 } }));
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    const host = await screen.findByLabelText("Hostname");
    await user.clear(host);
    await user.type(host, "web1.lab");
    await user.clear(screen.getByLabelText("File-table slots"));
    await user.type(screen.getByLabelText("Disk quota"), "2G");
    await user.click(screen.getByRole("button", { name: "Save" }));
    // Emptying the file-table field removes that limit: confirmed first.
    expect(await screen.findByTestId("removed-limits")).toHaveTextContent("File-table slots");
    await user.click(screen.getByRole("button", { name: "Remove the limit and save" }));
    await waitFor(() => expect(onTask).toHaveBeenCalled());
    expect(calls.find((c) => c.key === PUT)?.body).toEqual({
      hostname: "web1.lab",
      options: { sysvipc: false, mlock: false },
      limits: { memory: 512 << 20, disk_quota: 2 * 2 ** 30 },
    });
  });

  it("lists every limit a save would remove, and sends nothing on Keep editing", async () => {
    const { calls } = renderEdit(jail({ limits: { memory: 512 << 20, cpu_percent: 50, disk_quota: 1 << 30 } }));
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.clear(await screen.findByLabelText("Memory (soft)"));
    await user.clear(screen.getByLabelText("Disk quota"));
    await user.click(screen.getByRole("button", { name: "Save" }));
    const list = await screen.findByTestId("removed-limits");
    expect(list).toHaveTextContent("Memory (soft)");
    expect(list).toHaveTextContent("Disk quota");
    expect(list).not.toHaveTextContent("CPU");
    expect(screen.getByText("Remove 2 limits?")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByTestId("removed-limits")).not.toBeInTheDocument();
    expect(calls.some((c) => c.key === PUT)).toBe(false);
  });

  it("finds removed limits", () => {
    expect(removedLimits({ memory: 1 << 30, open_files: 128 }, { memory: 1 << 30 })).toEqual(["open_files"]);
    expect(removedLimits({ memory: 1 << 30 }, { memory: 2 << 20 })).toEqual([]);
    expect(removedLimits({}, { disk_quota: 1 << 30 })).toEqual([]);
  });

  it("before the first start, edits addressing, nameservers, and the key", async () => {
    const { calls, onTask } = renderEdit(jail({ nics: [staticNIC], nameservers: ["192.0.2.53"] }));
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    const addr = await screen.findByLabelText("Address");
    await user.clear(addr);
    await user.type(addr, "192.0.2.20/24");
    const ns = screen.getByLabelText("Nameservers");
    await user.clear(ns);
    await user.type(ns, "192.0.2.54");
    await user.type(await screen.findByLabelText("SSH key for root"), "ssh-ed25519 AAAA andy@merrick");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onTask).toHaveBeenCalled());
    const body = calls.find((c) => c.key === PUT)?.body as Record<string, unknown>;
    expect(body.nics).toEqual([{ bridge: "bridge0", ipv4: { mode: "static", address: "192.0.2.20/24" } }]);
    expect(body.nameservers).toEqual(["192.0.2.54"]);
    expect(body.ssh_authorized_key).toBe("ssh-ed25519 AAAA andy@merrick");
  });

  it("offers no key for a template without sshd", async () => {
    renderEdit(jail(), {}, false);
    await screen.findByRole("dialog");
    expect(await screen.findByTestId("no-sshd")).toBeInTheDocument();
    expect(screen.queryByLabelText("SSH key for root")).not.toBeInTheDocument();
  });

  it("after the first start, changes only the bridge and keeps the addressing", async () => {
    const started = jail({ started_once: true, nics: [{ ...staticNIC, ipv4: { mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" } }] });
    const { calls, onTask } = renderEdit(started);
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    const bridge = await screen.findByLabelText("skep0 bridge");
    expect(screen.queryByLabelText("Address")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Nameservers")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("SSH key for root")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add network interface" })).not.toBeInTheDocument();
    await screen.findByRole("option", { name: "bridge1" });
    await user.selectOptions(bridge, "bridge1");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onTask).toHaveBeenCalled());
    const body = calls.find((c) => c.key === PUT)?.body as Record<string, unknown>;
    expect(body.nics).toEqual([{ bridge: "bridge1", ipv4: { mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" } }]);
    expect(body).not.toHaveProperty("nameservers");
    expect(body).not.toHaveProperty("ssh_authorized_key");
    // The template is not asked for: the key cannot change any more.
    expect(calls.some((c) => c.key === "GET /api/v1/storage/templates/t1")).toBe(false);
  });

  it("shows the API's refusal with a hint", async () => {
    renderEdit(jail(), { [PUT]: { status: 409, body: { error: { code: "container_not_stopped", message: "the jail runs" } } } });
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Stop the jail first/)).toBeInTheDocument();
  });

  it("shows a disk quota below usage with its message and hint (amendment w)", async () => {
    const message = "limits.disk_quota (104857600 bytes) is below what the jail's root already references (138412032 bytes)";
    renderEdit(jail(), {
      [PUT]: {
        status: 422,
        body: { error: { code: "container_disk_quota_below_usage", message, details: { disk_quota: 104857600, referenced_bytes: 138412032 } } },
      },
    });
    await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Save" }));
    expect(await screen.findByText(/below what the jail's root holds/)).toBeInTheDocument();
    expect(screen.getByText(/138412032 bytes/)).toBeInTheDocument();
  });
});

describe("the Edit button", () => {
  const base = (c: Container, perms: string[]) => {
    fakeApi(perms, {
      [`GET /api/v1/containers/${ID}`]: { status: 200, body: c },
      [`GET /api/v1/containers/${ID}/snapshots`]: { status: 200, body: { items: [] } },
      [`GET /api/v1/containers/${ID}/console-log`]: { status: 200, body: { text: "", truncated: false } },
    });
    renderAt(<ContainerDetailPage />, `/containers/${ID}`, "/containers/:id");
  };

  it("is hidden without container:write", async () => {
    base(jail(), ["container:read", "container:power"]);
    await screen.findByRole("button", { name: "Start" });
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  });

  it("is disabled unless the jail is stopped", async () => {
    base(jail({ state: "running" }), ["container:read", "container:write"]);
    expect(await screen.findByRole("button", { name: "Edit" })).toBeDisabled();
  });
});
