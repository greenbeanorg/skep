import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ContainersPage, addressing } from "./ContainersPage";
import { dhcpNIC, jail, staticNIC } from "./fixtures";
import { fakeApi, renderAt } from "./testing";

const LIST = { "GET /api/v1/containers?limit=200": { status: 200, body: { items: [jail({ nics: [staticNIC] })] } } };

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ContainersPage", () => {
  it("lists jails with their addressing", async () => {
    fakeApi(["container:read", "system:read"], LIST);
    renderAt(<ContainersPage />);
    expect(await screen.findByRole("link", { name: "web1" })).toHaveAttribute("href", `/containers/${jail().id}`);
    expect(screen.getByText("bridge0 192.0.2.10/24")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create container" })).not.toBeInTheDocument();
  });

  it("offers create with container:write, and shows RACCT off", async () => {
    fakeApi(["container:read", "container:write", "system:read"], LIST, false);
    renderAt(<ContainersPage />);
    expect(await screen.findByRole("button", { name: "Create container" })).toBeInTheDocument();
    expect(await screen.findByTestId("racct-off")).toBeInTheDocument();
  });

  it("summarises addressing", () => {
    expect(addressing(jail())).toBe("none");
    expect(addressing(jail({ nics: [dhcpNIC] }))).toBe("bridge0 DHCP");
  });
});
