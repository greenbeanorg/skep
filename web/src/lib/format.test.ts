import { describe, expect, it } from "vitest";

import { formatAgo, formatBytes, formatDuration, formatElapsed, percent, shortId } from "./format";

describe("formatBytes", () => {
  it("uses binary units", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1536 * 1024 * 1024)).toBe("1.5 GiB");
    expect(formatBytes(200 * 1024 * 1024)).toBe("200 MiB");
  });
  it("rejects nonsense", () => {
    expect(formatBytes(-1)).toBe("unknown");
    expect(formatBytes(Number.NaN)).toBe("unknown");
  });
});

describe("formatDuration", () => {
  it("picks the two largest units", () => {
    expect(formatDuration(42)).toBe("42s");
    expect(formatDuration(423)).toBe("7m 3s");
    expect(formatDuration(5 * 3600 + 12 * 60)).toBe("5h 12m");
    expect(formatDuration(3 * 86400 + 4 * 3600 + 5)).toBe("3d 4h");
    expect(formatDuration(1800)).toBe("30m");
    expect(formatDuration(12 * 3600)).toBe("12h");
  });
});

describe("formatElapsed", () => {
  it("shows milliseconds for short spans", () => {
    expect(formatElapsed("2026-11-12T06:25:30.328Z", "2026-11-12T06:25:30.340Z")).toBe("12 ms");
    expect(formatElapsed("2026-11-12T06:25:30.000Z", "2026-11-12T06:25:32.500Z")).toBe("2.5 s");
  });
  it("is empty when a time is missing or reversed", () => {
    expect(formatElapsed(undefined, "2026-11-12T06:25:30.340Z")).toBe("");
    expect(formatElapsed("2026-11-12T06:25:31.000Z", "2026-11-12T06:25:30.000Z")).toBe("");
  });
});

describe("formatAgo", () => {
  it("is relative to the given time", () => {
    const now = Date.parse("2026-11-12T06:30:00.000Z");
    expect(formatAgo("2026-11-12T06:29:58.000Z", now)).toBe("just now");
    expect(formatAgo("2026-11-12T06:25:00.000Z", now)).toBe("5m ago");
  });
});

describe("percent", () => {
  it("clamps and handles a zero total", () => {
    expect(percent(1, 4)).toBe(25);
    expect(percent(5, 4)).toBe(100);
    expect(percent(1, 0)).toBeNull();
  });
});

describe("shortId", () => {
  it("shortens UUIDs only", () => {
    expect(shortId("97fc6968-7e82-49c9-bda8-9026775ca964")).toBe("97fc6968");
    expect(shortId("system")).toBe("system");
  });
});
