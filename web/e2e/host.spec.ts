import { expect, test, type Page } from "@playwright/test";

// A read-only pass over the interface on a real host (external mode). It
// changes nothing on the host: it logs in as an existing user, opens every
// section that user's role shows, and logs out. It creates no user, VM,
// container or task, uses no fixed password, and expects none of the local
// stack's fixtures (the "andy" user, the test.sleep task, devpool).
//
// Run with scripts/web-e2e.sh in external mode, which trusts the host only
// by its key pin:
//
//   SKEP_E2E_EXTERNAL=1 SKEP_E2E_LISTEN=host:8443 SKEP_E2E_PIN=sha256:<hex> \
//   SKEP_E2E_HOST_USER=<existing user> SKEP_E2E_HOST_PASSWORD=<its password> \
//   scripts/web-e2e.sh e2e/host.spec.ts
//
// Without SKEP_E2E_EXTERNAL=1 and both credentials it is skipped. The
// password comes from the environment only and is never printed. Each run
// is one successful login (audited, and counted by the login rate limit).

const external = process.env.SKEP_E2E_EXTERNAL === "1";
const user = process.env.SKEP_E2E_HOST_USER ?? "";
const password = process.env.SKEP_E2E_HOST_PASSWORD ?? "";

test.skip(!external || user === "" || password === "", "host mode needs SKEP_E2E_EXTERNAL=1, SKEP_E2E_HOST_USER and SKEP_E2E_HOST_PASSWORD");

/** Collects CSP violations and console errors for the lifetime of a page. */
function watch(page: Page): { violations: string[]; errors: string[] } {
  const violations: string[] = [];
  const errors: string[] = [];
  void page.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (e) => {
      console.error(`CSP violation: ${e.violatedDirective} blocked ${e.blockedURI}`);
    });
  });
  page.on("console", (msg) => {
    const text = msg.text();
    if (text.includes("Content Security Policy") || text.includes("CSP violation")) violations.push(text);
    // The session check before login answers 401; that is expected.
    else if (msg.type() === "error" && !/status of 401/.test(text)) errors.push(text);
  });
  page.on("pageerror", (err) => errors.push(String(err)));
  return { violations, errors };
}

test("headers: the interface is no-store with its CSP, and the API stays JSON", async ({ request }) => {
  const root = await request.get("/nodes/any");
  expect(root.status()).toBe(200);
  expect(root.headers()["content-type"]).toContain("text/html");
  expect(root.headers()["cache-control"]).toBe("no-store");
  expect(root.headers()["content-security-policy"]).toContain("script-src 'self'");
  expect(root.headers()["referrer-policy"]).toBe("no-referrer");
  const api = await request.get("/api/v1/nope");
  expect(api.status()).toBe(404);
  expect(api.headers()["content-type"]).toContain("application/json");
});

test("read-only tour: log in, open every section the role shows, log out", async ({ page }) => {
  const { violations, errors } = watch(page);
  await page.goto("/");
  await expect(page.getByRole("form", { name: "Log in" })).toBeVisible();
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Log in" }).click();

  const nav = page.getByRole("navigation", { name: "Sections" });
  await expect(nav).toBeVisible();

  // The session cookie's flags; nothing about the session in storage.
  const session = (await page.context().cookies()).find((c) => c.name === "__Host-skep_session");
  expect(session?.httpOnly).toBe(true);
  expect(session?.secure).toBe(true);
  expect(session?.sameSite).toBe("Strict");
  const keys = await page.evaluate(() => Object.keys(localStorage).concat(Object.keys(sessionStorage)));
  expect(keys.filter((k) => k !== "skep-theme")).toEqual([]);

  // Every link the role shows: a built section has its title as the page's
  // h1; a planned one says so. Each page's code is fetched on first visit
  // (pages are split by route), so this also checks that every chunk loads
  // under the CSP.
  const links = nav.getByRole("link");
  const count = await links.count();
  expect(count).toBeGreaterThan(0);
  for (let i = 0; i < count; i++) {
    // The label is the link's first text; a planned section adds a tag.
    const label = (await links.nth(i).locator("span.truncate").innerText()).trim();
    await test.step(label, async () => {
      await links.nth(i).click();
      await expect(page.getByText("This page could not be loaded")).toHaveCount(0);
      const heading = page.getByRole("heading", { level: 1, name: label, exact: true });
      const planned = page.getByText("Not built yet");
      await expect(heading.or(planned).first()).toBeVisible();
    });
  }

  await page.getByRole("button", { name: "Account menu" }).click();
  await page.getByRole("menuitem", { name: "Your account" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Your account" })).toBeVisible();

  await page.getByRole("button", { name: "Account menu" }).click();
  await page.getByRole("menuitem", { name: "Log out" }).click();
  await expect(page.getByText("You are logged out")).toBeVisible();
  expect((await page.request.get("/api/v1/auth/session")).status()).toBe(401);

  expect(violations, "CSP violations").toEqual([]);
  expect(errors, "console errors").toEqual([]);
});
