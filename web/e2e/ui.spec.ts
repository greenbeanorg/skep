import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

// The browser tests ADR 0008 asks for: log in, the dashboard renders, a
// viewer cannot see Users, cancel a task, log out, an expired session
// returns to the login page and back, and the CSP produces no violations.
// Every page is also photographed in both themes, for review.

const ADMIN = { username: "andy", password: process.env.SKEP_E2E_PASSWORD ?? "correct horse battery staple" };
const VIEWER = { username: "vera", password: "viewer passphrase for tests" };

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
    // The browser logs every non-2xx response as an error; an expected
    // 401 (the session check before login, a wrong password) is not one.
    else if (msg.type() === "error" && !/status of 401/.test(text)) errors.push(text);
  });
  page.on("pageerror", (err) => errors.push(String(err)));
  return { violations, errors };
}

async function login(page: Page, who: { username: string; password: string }) {
  await expect(page.getByRole("form", { name: "Log in" })).toBeVisible();
  await page.getByLabel("Username").fill(who.username);
  await page.getByLabel("Password").fill(who.password);
  await page.getByRole("button", { name: "Log in" }).click();
}

async function loginAPI(request: APIRequestContext, who: { username: string; password: string }) {
  const res = await request.post("/api/v1/auth/login", {
    data: { username: who.username, password: who.password },
    headers: { Origin: new URL(test.info().project.use.baseURL ?? "https://127.0.0.1:8443").origin },
  });
  expect(res.status(), await res.text()).toBe(200);
}

/** Ensures the viewer exists (idempotent), using the request context's admin cookie. */
async function ensureViewer(request: APIRequestContext) {
  const origin = new URL(test.info().project.use.baseURL ?? "https://127.0.0.1:8443").origin;
  const users = await request.get("/api/v1/users");
  expect(users.status()).toBe(200);
  const existing = ((await users.json()) as { items: { id: string; username: string; disabled: boolean }[] }).items.find(
    (u) => u.username === VIEWER.username,
  );
  if (existing) {
    if (existing.disabled) {
      const res = await request.put(`/api/v1/users/${existing.id}`, { data: { disabled: false }, headers: { Origin: origin } });
      expect(res.status()).toBe(200);
    }
    return existing.id;
  }
  const res = await request.post("/api/v1/users", {
    data: { username: VIEWER.username, display_name: "Vera Viewer", password: VIEWER.password, role: "viewer" },
    headers: { Origin: origin },
  });
  expect(res.status(), await res.text()).toBe(201);
  return ((await res.json()) as { id: string }).id;
}

async function shoot(page: Page, name: string) {
  for (const theme of ["light", "dark"] as const) {
    await page.evaluate((t) => {
      localStorage.setItem("skep-theme", t);
      document.documentElement.setAttribute("data-theme", t);
    }, theme);
    await page.waitForTimeout(150);
    await page.screenshot({ path: `test-results/screens/${name}-${theme}.png`, fullPage: true });
  }
  await page.evaluate(() => localStorage.removeItem("skep-theme"));
}

test.describe.configure({ mode: "serial" });

test("the interface is served with its headers and the API stays JSON", async ({ request }) => {
  const root = await request.get("/nodes/node-1");
  expect(root.status()).toBe(200);
  expect(root.headers()["content-type"]).toContain("text/html");
  expect(root.headers()["cache-control"]).toBe("no-store");
  expect(root.headers()["content-security-policy"]).toContain("script-src 'self'");
  expect(root.headers()["referrer-policy"]).toBe("no-referrer");
  const api = await request.get("/api/v1/nope");
  expect(api.status()).toBe(404);
  expect(api.headers()["content-type"]).toContain("application/json");
});

test("login, dashboard, every page in both themes, logout", async ({ page }) => {
  const { violations, errors } = watch(page);
  await page.goto("/");
  await shoot(page, "login");

  // A wrong password: one message, password cleared.
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password").fill("not the password");
  await page.getByRole("button", { name: "Log in" }).click();
  await expect(page.getByText("The username or password is not correct.")).toBeVisible();
  await expect(page.getByLabel("Password")).toHaveValue("");
  await shoot(page, "login-error");

  await page.getByLabel("Password").fill(ADMIN.password);
  await page.getByRole("button", { name: "Log in" }).click();

  // The dashboard, with the health checks and the recent tasks.
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Health checks" })).toBeVisible();
  await expect(page.getByRole("link", { name: /test\.sleep/ }).first()).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: "Users and permissions" })).toBeVisible();
  await shoot(page, "dashboard");

  // Nothing about the session in storage.
  const keys = await page.evaluate(() => Object.keys(localStorage).concat(Object.keys(sessionStorage)));
  expect(keys.filter((k) => k !== "skep-theme")).toEqual([]);
  const cookies = await page.context().cookies();
  const session = cookies.find((c) => c.name === "__Host-skep_session");
  expect(session?.httpOnly).toBe(true);
  expect(session?.secure).toBe(true);
  expect(session?.sameSite).toBe("Strict");

  const nav = page.getByRole("navigation", { name: "Sections" });
  await nav.getByRole("link", { name: "Nodes" }).click();
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Processor" })).toBeVisible();
  await shoot(page, "nodes");

  await nav.getByRole("link", { name: "Tasks" }).click();
  await expect(page.getByRole("heading", { name: "Tasks" })).toBeVisible();
  await expect(page.getByRole("link", { name: /test\.sleep/ }).first()).toBeVisible();
  await shoot(page, "tasks");

  await page.getByRole("link", { name: /test\.sleep/ }).first().click();
  await expect(page.getByRole("heading", { name: "test.sleep" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Events" })).toBeVisible();
  await shoot(page, "task-detail");

  await nav.getByRole("link", { name: "Users and permissions" }).click();
  await expect(page.getByRole("heading", { name: "Users and permissions" })).toBeVisible();
  await expect(page.getByRole("cell", { name: /andy/ })).toBeVisible();
  await shoot(page, "users");
  await page.getByRole("link", { name: "Roles" }).click();
  await expect(page.getByText("Every permission.", { exact: true })).toBeVisible();
  await shoot(page, "roles");
  await page.getByRole("link", { name: "Access audit log" }).click();
  await expect(page.getByText("auth.login").first()).toBeVisible();
  await shoot(page, "audit");

  await nav.getByRole("link", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
  await expect(page.getByText(/sha256:[0-9a-f]{64}/)).toBeVisible();
  await shoot(page, "settings");

  await nav.getByRole("link", { name: "Containers" }).click();
  await expect(page.getByText("Not built yet")).toBeVisible();
  await shoot(page, "planned");

  await page.getByRole("button", { name: "Account menu" }).click();
  await page.getByRole("menuitem", { name: "Your account" }).click();
  await expect(page.getByRole("heading", { name: "Your account" })).toBeVisible();
  await shoot(page, "account");

  // Log out: the login page says so, and the session is gone on the server.
  await page.getByRole("button", { name: "Account menu" }).click();
  await page.getByRole("menuitem", { name: "Log out" }).click();
  await expect(page.getByText("You are logged out")).toBeVisible();
  const check = await page.request.get("/api/v1/auth/session");
  expect(check.status()).toBe(401);

  expect(violations, "CSP violations").toEqual([]);
  expect(errors, "console errors").toEqual([]);
});

test("a viewer does not see Users and cannot open it", async ({ browser, request }) => {
  await loginAPI(request, ADMIN);
  await ensureViewer(request);

  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const { violations } = watch(page);
  await page.goto("/users");
  await login(page, VIEWER);
  // Not allowed, at the same address.
  await expect(page.getByText("Your role does not allow this")).toBeVisible();
  expect(new URL(page.url()).pathname).toBe("/users");
  const nav = page.getByRole("navigation", { name: "Sections" });
  await expect(nav.getByRole("link", { name: "Dashboard" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Users and permissions" })).toHaveCount(0);
  await shoot(page, "viewer-forbidden");
  await nav.getByRole("link", { name: "Tasks" }).click();
  // No cancel for a viewer.
  await page.getByRole("link", { name: /test\.sleep/ }).first().click();
  await expect(page.getByRole("heading", { name: "test.sleep" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel task" })).toHaveCount(0);

  // VMs: readable, with no power, console, create, or delete.
  const origin = new URL(test.info().project.use.baseURL ?? "https://127.0.0.1:8443").origin;
  const res = await request.post("/api/v1/vms", {
    data: { name: "viewer-sees", cpu_count: 1, memory: "256M", disks: [{ size: "1G", pool: "devpool", thin: true }] },
    headers: { Origin: origin },
  });
  expect(res.status(), await res.text()).toBe(202);
  const { vm } = (await res.json()) as { vm: { id: string } };
  await nav.getByRole("link", { name: "Virtual machines" }).click();
  await expect(page.getByRole("link", { name: "viewer-sees" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create VM" })).toHaveCount(0);
  await page.getByRole("link", { name: "viewer-sees" }).click();
  await expect(page.getByRole("heading", { name: "viewer-sees" })).toBeVisible();
  await expect(page.getByText("Configuration")).toBeVisible();
  for (const name of ["Start", "Console", "Delete"]) {
    await expect(page.getByRole("button", { name, exact: true })).toHaveCount(0);
  }
  expect(violations).toEqual([]);
  await context.close();
  const del = await request.delete(`/api/v1/vms/${vm.id}`, { headers: { Origin: origin } });
  expect([202, 409]).toContain(del.status());
});

test("an expired session returns to the login page and back to the same address", async ({ browser, request }) => {
  await loginAPI(request, ADMIN);
  const viewerID = await ensureViewer(request);
  const origin = new URL(test.info().project.use.baseURL ?? "https://127.0.0.1:8443").origin;

  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  await page.goto("/tasks");
  await login(page, VIEWER);
  await expect(page.getByRole("heading", { name: "Tasks" })).toBeVisible();

  // The administrator disables the viewer: every session of the viewer
  // ends. The page's next background refresh gets 401.
  let res = await request.put(`/api/v1/users/${viewerID}`, { data: { disabled: true }, headers: { Origin: origin } });
  expect(res.status()).toBe(200);
  await expect(page.getByText("Your session has ended")).toBeVisible({ timeout: 20_000 });
  expect(new URL(page.url()).pathname).toBe("/tasks");
  await shoot(page, "login-expired");

  res = await request.put(`/api/v1/users/${viewerID}`, { data: { disabled: false }, headers: { Origin: origin } });
  expect(res.status()).toBe(200);
  await login(page, VIEWER);
  await expect(page.getByRole("heading", { name: "Tasks" })).toBeVisible();
  expect(new URL(page.url()).pathname).toBe("/tasks");
  await context.close();
});

test("an administrator cancels the running task", async ({ page }) => {
  const { violations } = watch(page);
  await page.goto("/tasks");
  await login(page, ADMIN);
  await page.getByRole("link", { name: /test\.sleep/ }).first().click();
  await expect(page.getByRole("heading", { name: "test.sleep" })).toBeVisible();
  await page.getByRole("button", { name: "Cancel task" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: /Cancel test\.sleep/ })).toBeVisible();
  await shoot(page, "task-cancel");
  await dialog.getByRole("button", { name: "Cancel task" }).click();
  await expect(page.getByText("cancelled", { exact: true }).first()).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole("button", { name: "Cancel task" })).toHaveCount(0);

  // The access audit log names the actor.
  await page.goto("/users/audit");
  await expect(page.getByText("task.cancel").first()).toBeVisible();
  expect(violations).toEqual([]);
});

test("an administrator creates a VM, starts it, types in its console, stops and deletes it", async ({ page }) => {
  const { violations, errors } = watch(page);
  await page.goto("/vms");
  await login(page, ADMIN);
  await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
  await expect(page.getByText("Simulated host")).toBeVisible();

  await page.getByRole("button", { name: "Create VM" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Name").fill("e2e-vm");
  await dialog.getByLabel("Memory").fill("512M");
  await dialog.getByLabel("Disk size").fill("2G");
  await dialog.getByLabel("Install media").selectOption("test-install.iso");
  await shoot(page, "vm-create");
  await dialog.getByRole("button", { name: "Create" }).click();

  // The detail page of the new VM.
  await expect(page.getByRole("heading", { name: "e2e-vm" })).toBeVisible();
  await expect(page.getByText("test-install.iso")).toBeVisible();
  await expect(page.getByRole("button", { name: "Start" })).toBeVisible({ timeout: 15_000 });
  await page.getByRole("button", { name: "Start" }).click();
  await expect(page.getByText("running", { exact: true }).first()).toBeVisible({ timeout: 15_000 });

  await page.getByRole("button", { name: "Console" }).click();
  await expect(page.getByText("connected", { exact: true })).toBeVisible();
  // The fake guest's banner from before the attachment, then an echo.
  const consoleBox = page.getByTestId("serial-console");
  await expect(consoleBox).toContainText("simulated serial console", { timeout: 10_000 });
  await consoleBox.click();
  await page.keyboard.type("hello from the browser");
  await expect(consoleBox).toContainText("hello from the browser");
  await shoot(page, "vm-console");

  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.getByText("stopped", { exact: true }).first()).toBeVisible({ timeout: 15_000 });
  // Media can be detached while it is stopped.
  await page.getByRole("button", { name: "Detach" }).click();
  await expect(page.getByText("None attached.")).toBeVisible({ timeout: 10_000 });
  await shoot(page, "vm-detail");

  await page.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete VM and disks" }).click();
  await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
  await expect(page.getByRole("link", { name: "e2e-vm" })).toHaveCount(0, { timeout: 15_000 });

  expect(violations, "CSP violations").toEqual([]);
  expect(errors, "console errors").toEqual([]);
});
