import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { CreateContainerDialog } from "./CreateContainerDialog";
import { jail, template } from "./fixtures";
import { fakeApi, renderAt } from "./testing";

const ALL = ["*"];
const templates = { status: 200, body: { items: [template(), template({ id: "t2", name: "building", provisioned: false })] } };

afterEach(() => {
  vi.unstubAllGlobals();
});

function renderDialog() {
  const onCreated = vi.fn();
  renderAt(<CreateContainerDialog onClose={() => {}} onCreated={onCreated} />);
  return onCreated;
}

describe("CreateContainerDialog", () => {
  it("offers only built templates and shows their packages", async () => {
    fakeApi(ALL, { "GET /api/v1/storage/templates?limit=200": templates });
    renderDialog();
    const select = await screen.findByLabelText("Template");
    await waitFor(() => expect(select.querySelectorAll("option")).toHaveLength(1));
    expect(screen.getByText(/Packages: FreeBSD-runtime, FreeBSD-ssh/)).toBeInTheDocument();
  });

  it("sends the template, a static NIC, nameservers, a key, and the limits", async () => {
    const created = { container: jail(), task: { id: "t9", type: "container.create", status: "pending" } };
    const calls = fakeApi(ALL, {
      "GET /api/v1/storage/templates?limit=200": templates,
      "POST /api/v1/containers": { status: 202, body: created },
    });
    const onCreated = renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.type(screen.getByLabelText("Name"), "web1");
    await user.click(await screen.findByRole("button", { name: "Add network interface" }));
    await user.selectOptions(screen.getByLabelText("IPv4"), "static");
    await user.type(screen.getByLabelText("Address"), "192.0.2.10/24");
    await user.type(screen.getByLabelText("Gateway (optional)"), "192.0.2.1");
    await user.type(screen.getByLabelText("Nameservers"), "192.0.2.53, 2001:db8::53");
    await user.type(screen.getByLabelText("SSH key for root (optional)"), "ssh-ed25519 AAAAC3Nza andy@merrick");
    await user.type(screen.getByLabelText("Memory (soft)"), "512M");
    await user.type(screen.getByLabelText("File-table slots"), "1024");
    await user.type(screen.getByLabelText("Disk quota"), "1G");
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(onCreated).toHaveBeenCalled());
    const post = calls.find((c) => c.key === "POST /api/v1/containers");
    expect(post?.body).toEqual({
      name: "web1",
      template_id: "t1",
      limits: { memory: 512 << 20, open_files: 1024, disk_quota: 1 << 30 },
      ssh_authorized_key: "ssh-ed25519 AAAAC3Nza andy@merrick",
      nics: [{ bridge: "bridge0", ipv4: { mode: "static", address: "192.0.2.10/24", gateway: "192.0.2.1" } }],
      nameservers: ["192.0.2.53", "2001:db8::53"],
    });
  });

  it("explains memory as soft, CPU as approximate, and open_files as slots", async () => {
    fakeApi(ALL, { "GET /api/v1/storage/templates?limit=200": templates });
    renderDialog();
    await screen.findByText(/Packages:/);
    expect(screen.getByText(/not a cap: FreeBSD does not refuse an allocation/)).toBeInTheDocument();
    expect(screen.getByLabelText("CPU % (approximate)")).toBeInTheDocument();
    expect(screen.getByText(/At most 400 on this host/)).toBeInTheDocument();
    expect(screen.getByText(/Counts descriptor-table slots, not open files/)).toBeInTheDocument();
    expect(screen.getByText(/What the jail may lock with mlock\(2\)/)).toBeInTheDocument();
  });

  it("hides nameservers while a NIC uses DHCP", async () => {
    fakeApi(ALL, { "GET /api/v1/storage/templates?limit=200": templates });
    renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.click(await screen.findByRole("button", { name: "Add network interface" }));
    expect(screen.queryByLabelText("Nameservers")).not.toBeInTheDocument();
    expect(screen.getByText(/dhclient writes the jail's resolv.conf/)).toBeInTheDocument();
  });

  it("refuses mlock without a locked-memory limit", async () => {
    fakeApi(ALL, { "GET /api/v1/storage/templates?limit=200": templates });
    renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.type(screen.getByLabelText("Name"), "db1");
    await user.click(screen.getByLabelText(/Lock memory/));
    expect(screen.getByText("Locking memory needs a locked-memory limit.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    await user.type(screen.getByLabelText("Locked memory"), "64M");
    expect(screen.getByRole("button", { name: "Create" })).toBeEnabled();
  });

  it("shows RACCT off and disables the RCTL limits, not the disk quota", async () => {
    fakeApi(ALL, { "GET /api/v1/storage/templates?limit=200": templates }, false);
    renderDialog();
    expect(await screen.findByTestId("racct-off")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Memory (soft)")).toBeDisabled());
    expect(screen.getByLabelText("File-table slots")).toBeDisabled();
    expect(screen.getByLabelText("Disk quota")).toBeEnabled();
  });

  it("adds a hint to an SSH refusal", async () => {
    fakeApi(ALL, {
      "GET /api/v1/storage/templates?limit=200": templates,
      "POST /api/v1/containers": {
        status: 422,
        body: { error: { code: "container_ssh_unavailable", message: "the template minimal-151 has no sshd" } },
      },
    });
    renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.type(screen.getByLabelText("Name"), "web1");
    await user.type(screen.getByLabelText("SSH key for root (optional)"), "ssh-ed25519 AAAA x");
    await user.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText(/Choose a template built with FreeBSD-ssh/)).toBeInTheDocument();
  });

  it("shows a quota below the template's size with its message and hint (amendment x)", async () => {
    const message = "limits.disk_quota (67108864 bytes) is below the template's size (81788928 bytes)";
    fakeApi(ALL, {
      "GET /api/v1/storage/templates?limit=200": templates,
      "POST /api/v1/containers": {
        status: 422,
        body: { error: { code: "container_disk_quota_below_usage", message, details: { disk_quota: 67108864, referenced_bytes: 81788928 } } },
      },
    });
    renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.type(screen.getByLabelText("Name"), "web1");
    await user.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText(/below the template's size, which a new jail's root holds/)).toBeInTheDocument();
    expect(screen.getByText(/81788928 bytes/)).toBeInTheDocument();
  });

  it("shows the SSH key field only for a template with sshd", async () => {
    const calls = fakeApi(ALL, {
      "GET /api/v1/storage/templates?limit=200": {
        status: 200,
        body: { items: [template(), template({ id: "t3", name: "nossh", has_sshd: false, packages: ["FreeBSD-runtime"] })] },
      },
      "POST /api/v1/containers": { status: 202, body: { container: jail(), task: { id: "t9", type: "container.create", status: "pending" } } },
    });
    renderDialog();
    const user = userEvent.setup();
    await screen.findByText(/Packages:/);
    await user.type(screen.getByLabelText("SSH key for root (optional)"), "ssh-ed25519 AAAA x");
    await user.selectOptions(screen.getByLabelText("Template"), "t3");
    expect(screen.queryByLabelText("SSH key for root (optional)")).not.toBeInTheDocument();
    expect(screen.getByTestId("no-sshd")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Name"), "web1");
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(calls.some((c) => c.key === "POST /api/v1/containers")).toBe(true));
    // A key typed for another template is not sent.
    expect(calls.find((c) => c.key === "POST /api/v1/containers")?.body).toEqual({ name: "web1", template_id: "t3" });
  });

  it("says so when the role cannot list templates", async () => {
    fakeApi(["container:write", "system:read"], {});
    renderDialog();
    expect(await screen.findByText(/lacks storage:read/)).toBeInTheDocument();
  });
});
