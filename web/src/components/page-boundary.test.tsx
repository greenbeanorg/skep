import { render, screen } from "@testing-library/react";
import { lazy } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { PageBoundary } from "./page-boundary";

afterEach(() => vi.restoreAllMocks());

describe("PageBoundary", () => {
  it("shows a placeholder while a page's code loads, then the page", async () => {
    let resolve: (m: { default: () => React.ReactNode }) => void = () => {};
    const Page = lazy(() => new Promise<{ default: () => React.ReactNode }>((r) => (resolve = r)));
    render(
      <PageBoundary resetKey="/a">
        <Page />
      </PageBoundary>,
    );
    expect(screen.getByLabelText("Loading the page")).toBeInTheDocument();
    resolve({ default: () => <p>the page</p> });
    expect(await screen.findByText("the page")).toBeInTheDocument();
    expect(screen.queryByLabelText("Loading the page")).not.toBeInTheDocument();
  });

  it("offers a reload when a page's code cannot be fetched, and clears on the next navigation", async () => {
    // React logs the caught error; keep the test output clean.
    vi.spyOn(console, "error").mockImplementation(() => {});
    const Broken = lazy(() => Promise.reject(new TypeError("Failed to fetch dynamically imported module")));
    const { rerender } = render(
      <PageBoundary resetKey="/vms">
        <Broken />
      </PageBoundary>,
    );
    expect(await screen.findByText("This page could not be loaded")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();

    rerender(
      <PageBoundary resetKey="/tasks">
        <p>another page</p>
      </PageBoundary>,
    );
    expect(await screen.findByText("another page")).toBeInTheDocument();
    expect(screen.queryByText("This page could not be loaded")).not.toBeInTheDocument();
  });
});
