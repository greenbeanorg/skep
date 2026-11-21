import { defineConfig, devices } from "@playwright/test";

// Browser tests against a real skep-api with the interface embedded and the
// agent's test build behind it (ADR 0008, decision 8). "make web-e2e" in
// the repository root builds everything and runs them through
// scripts/web-e2e.sh; scripts/e2e-stack.sh is the stack.
//
// skep-api's certificate is trusted by its public key pin, never by
// ignoring certificate errors (ADR 0010 amendment x): scripts/web-e2e.sh
// sets SKEP_E2E_PIN (sha256:<hex>, as `skep-api tls fingerprint` prints
// it), which pins Chromium here, and NODE_EXTRA_CA_CERTS to the
// certificate carrying that key, which Playwright's Node-side request
// client and the webServer health check verify against.
const listen = process.env.SKEP_E2E_LISTEN ?? "127.0.0.1:8443";
const baseURL = `https://${listen}`;
// SKEP_E2E_EXTERNAL=1 runs the tests against a skep-api that is already
// running at SKEP_E2E_LISTEN (for example on the FreeBSD test host, with
// the agent's test build behind it) instead of starting the local stack.
const external = process.env.SKEP_E2E_EXTERNAL === "1";

const pin = process.env.SKEP_E2E_PIN ?? "";
const hex = /^sha256:([0-9a-f]{64})$/.exec(pin)?.[1];
if (!hex || !process.env.NODE_EXTRA_CA_CERTS) {
  throw new Error(
    "the browser tests trust skep-api only by its key pin: run them with `make web-e2e` or scripts/web-e2e.sh, " +
      "which set SKEP_E2E_PIN (sha256:<hex> from `skep-api tls fingerprint`) and NODE_EXTRA_CA_CERTS",
  );
}

/** Chromium's form of the pin: the base64 of the SPKI's SHA-256. */
const spkiPin = Buffer.from(hex, "hex").toString("base64");

const pinned = { args: [`--ignore-certificate-errors-spki-list=${spkiPin}`] };

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
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    ...devices["Desktop Chrome"],
    launchOptions: pinned,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], launchOptions: pinned } }],
  webServer: external
    ? undefined
    : {
        command: "../scripts/e2e-stack.sh",
        url: `${baseURL}/api/v1/health`,
        reuseExistingServer: false,
        timeout: 90_000,
        stdout: "pipe",
        stderr: "pipe",
      },
});
