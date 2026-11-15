import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Snapshot, VM } from "@/api/types";
import { SessionProvider, useSession } from "@/app/session";

import { SnapshotsCard } from "./SnapshotsCard";
import { newerSnapshots, snapshotsUnsupported } from "./vm";

const vm: VM = {
  id: "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11",
  name: "web1",
  description: "",
  engine: "bhyve",
  firmware: "uefi",
  cpu_count: 1,
  memory_bytes: 1 << 30,
  provisioned: true,
  desired_state: "stopped",
  drift: false,
  observed: { state: "stopped" },
  node_id: "n1",
  config: {},
  disks: [{ id: "d0", slot: 0, bus: "virtio-blk", read_only: false, pool: "skeptest", pool_type: "zfs", size_bytes: 8 << 30 }],
  nics: [],
  created_at: "2026-11-15T00:00:00Z",
  updated_at: "2026-11-15T00:00:00Z",
} as unknown as VM;
const dirDisks: VM["disks"] = [{ id: "d0", slot: 0, bus: "virtio-blk", read_only: false, pool: "dirpool", pool_type: "directory" }];

function snap(id: string, name: string, extra: Partial<Snapshot> = {}): Snapshot {
  return {
    id,
    resource_kind: "vm",
    resource_id: vm.id,
    name,
    description: "",
    consistency: "consistent",
    provisioned: true,
    created_at: "2026-11-15T01:00:00Z",
    clones: [],
    ...extra,
  };
}

const older = snap("s1", "first");
const newer = snap("s2", "second", { consistency: "crash_consistent" });

function fakeApi(permissions: string[], replies: Record<string, { status: number; body?: unknown }>) {
  const calls: { key: string; body?: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const key = `${init.method ?? "GET"} ${url}`;
      calls.push({ key, body: init.body as string | undefined });
      if (key === "GET /api/v1/auth/session") {
        return Response.json({
          user: { id: "u1", username: "andy", display_name: "", role: "x", disabled: false, created_at: "", updated_at: "" },
          session: { kind: "cookie", created_at: "", expires_at: "2099-01-01T00:00:00Z", idle_timeout_seconds: 1800 },
          permissions,
        });
      }
      const r = replies[key] ?? { status: 404, body: { error: { code: "not_found", message: "no" } } };
      return new Response(JSON.stringify(r.body ?? {}), { status: r.status, headers: { "Content-Type": "application/json" } });
    }),
  );
  return calls;
}

function WhenLoggedIn({ children }: { children: React.ReactNode }) {
  const { state } = useSession();
  return state.status === "authenticated" ? children : null;
}

function renderCard(v: VM = vm) {
  const onTask = vi.fn();
  render(
    <MemoryRouter>
      <SessionProvider>
        <WhenLoggedIn>
          <SnapshotsCard vm={v} onTask={onTask} />
        </WhenLoggedIn>
      </SessionProvider>
    </MemoryRouter>,
  );
  return onTask;
}

const task = { id: "t1", type: "vm.snapshot.rollback", status: "pending", created_at: "", updated_at: "" };
const listPath = `GET /api/v1/vms/${vm.id}/snapshots`;

afterEach(() => vi.unstubAllGlobals());

describe("snapshot helpers", () => {
  it("finds the newer snapshots and directory pools", () => {
    expect(newerSnapshots([older, newer], older).map((s) => s.id)).toEqual(["s2"]);
    expect(newerSnapshots([older, newer], newer)).toEqual([]);
    expect(snapshotsUnsupported(vm)).toBe(false);
    expect(snapshotsUnsupported({ ...vm, disks: dirDisks })).toBe(true);
  });
});

describe("SnapshotsCard", () => {
  it("rolls back to an older snapshot only after the newer ones are confirmed destroyed", async () => {
    const calls = fakeApi(["*"], {
      [listPath]: { status: 200, body: { items: [older, newer] } },
      [`POST /api/v1/vms/${vm.id}/snapshots/s1/rollback`]: { status: 202, body: task },
    });
    const onTask = renderCard();
    const row = (await screen.findByText("first")).closest("tr")!;
    expect(screen.getByText("crash-consistent")).toBeInTheDocument();
    await userEvent.click(within(row).getByRole("button", { name: "Roll back" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/destroys second/)).toBeInTheDocument();
    const go = within(dialog).getByRole("button", { name: "Roll back" });
    expect(go).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("checkbox"));
    await userEvent.click(go);
    await waitFor(() => expect(onTask).toHaveBeenCalled());
    const post = calls.find((c) => c.key.startsWith("POST"));
    expect(JSON.parse(post!.body!)).toEqual({ destroy_newer: true });
  });

  it("shows the clone identity note before a clone is made", async () => {
    fakeApi(["*"], { [listPath]: { status: 200, body: { items: [older] } } });
    renderCard();
    await screen.findByText("first");
    await userEvent.click(screen.getAllByRole("button", { name: "Clone" })[0]!);
    const note = screen.getByTestId("clone-identity-note");
    expect(note).toHaveTextContent("same guest");
    expect(note).toHaveTextContent("SSH host keys");
    expect(note).toHaveTextContent("machine ID");
  });

  it("offers no writes to a viewer, and says when the pool has no snapshots", async () => {
    fakeApi(["vm:read"], { [listPath]: { status: 200, body: { items: [older] } } });
    renderCard({ ...vm, disks: dirDisks });
    await screen.findByText("first");
    expect(screen.queryByRole("button", { name: /Take snapshot|Clone|Roll back|Delete/ })).toBeNull();
    expect(screen.getByText(/directory pool, which has no snapshots/)).toBeInTheDocument();
  });

  it("refuses to roll back a running VM", async () => {
    fakeApi(["*"], { [listPath]: { status: 200, body: { items: [older] } } });
    renderCard({ ...vm, observed: { state: "running" } });
    const row = (await screen.findByText("first")).closest("tr")!;
    expect(within(row).getByRole("button", { name: "Roll back" })).toBeDisabled();
  });
});
