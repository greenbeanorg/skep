import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SerialConsole, webgl2Available } from "./SerialConsole";

afterEach(() => vi.unstubAllGlobals());

describe("SerialConsole without WebGL", () => {
  it("says the console is unavailable and opens no connection", () => {
    // jsdom has no WebGL, like a browser with it turned off.
    expect(webgl2Available()).toBe(false);
    const ws = vi.fn();
    vi.stubGlobal("WebSocket", ws);
    render(<SerialConsole vmId="0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11" />);
    expect(screen.getByText("console unavailable")).toBeInTheDocument();
    expect(screen.getByText(/needs WebGL 2/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reconnect" })).toBeInTheDocument();
    expect(ws).not.toHaveBeenCalled();
    // No terminal was built: nothing that could fall back to the DOM
    // renderer and its <style> elements.
    expect(document.querySelector(".xterm")).toBeNull();
    expect(document.querySelector("style")).toBeNull();
  });
});
