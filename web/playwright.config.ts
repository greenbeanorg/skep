import { defineConfig, devices } from "@playwright/test";

// Browser tests against a real skep-api with the interface embedded and the
// agent's test build behind it (ADR 0008, decision 8). "make web-e2e" in
// the repository root builds everything and runs them;
// scripts/e2e-stack.sh is the stack. The certificate is skep-api's
// self-signed one, so the browser is told to accept it: these are tests of
// the interface, not of the trust decision an operator makes.
const listen = process.env.SKEP_E2E_LISTEN ?? "127.0.0.1:8443";
const baseURL = `https://${listen}`;
// SKEP_E2E_EXTERNAL=1 runs the tests against a skep-api that is already
// running at SKEP_E2E_LISTEN (for example on the FreeBSD test host, with
// the agent's test build behind it) instead of starting the local stack.
const external = process.env.SKEP_E2E_EXTERNAL === "1";

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    ignoreHTTPSErrors: true,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    ...devices["Desktop Chrome"],
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: external
    ? undefined
    : {
        command: "../scripts/e2e-stack.sh",
        url: `${baseURL}/api/v1/health`,
        ignoreHTTPSErrors: true,
        reuseExistingServer: false,
        timeout: 90_000,
        stdout: "pipe",
        stderr: "pipe",
      },
});
