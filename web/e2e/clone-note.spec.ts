import { expect, test } from "@playwright/test";

// The clone identity check of ADR 0010 decision 16, against a real host:
// the clone form and the clone's page both say that a clone keeps the
// source's hostname, SSH host keys, and machine ID. It makes a linked
// clone of an existing VM from one of its snapshots, through the
// interface, and deletes the clone again. Skipped unless pointed at a
// host, with SKEP_E2E_EXTERNAL=1 and SKEP_E2E_LISTEN set:
//
//   SKEP_E2E_CLONE_USER, SKEP_E2E_CLONE_PASSWORD  an operator or admin
//   SKEP_E2E_CLONE_VM        the source VM's name (stopped or running)
//   SKEP_E2E_CLONE_SNAPSHOT  the name of one of its snapshots
//   SKEP_E2E_CLONE_NAME      the clone's name (default e2e-clone-note)

const env = process.env;
const ready = Boolean(env.SKEP_E2E_CLONE_USER && env.SKEP_E2E_CLONE_PASSWORD && env.SKEP_E2E_CLONE_VM && env.SKEP_E2E_CLONE_SNAPSHOT);

test("the clone form and the clone's page carry the clone identity note", async ({ page }) => {
  test.skip(!ready, "set SKEP_E2E_CLONE_* to run against a host");
  const cloneName = env.SKEP_E2E_CLONE_NAME ?? "e2e-clone-note";
  const errors: string[] = [];
  page.on("pageerror", (err) => errors.push(String(err)));

  await page.goto("/vms");
  await page.getByLabel("Username").fill(env.SKEP_E2E_CLONE_USER!);
  await page.getByLabel("Password").fill(env.SKEP_E2E_CLONE_PASSWORD!);
  await page.getByRole("button", { name: "Log in" }).click();
  await page.getByRole("link", { name: env.SKEP_E2E_CLONE_VM!, exact: true }).click();
  await expect(page.getByRole("heading", { name: env.SKEP_E2E_CLONE_VM!, exact: true })).toBeVisible();

  const row = page.getByRole("row", { name: new RegExp(env.SKEP_E2E_CLONE_SNAPSHOT!) });
  await row.getByRole("button", { name: "Clone" }).click();
  const dialog = page.getByRole("dialog");
  const formNote = dialog.getByTestId("clone-identity-note");
  await expect(formNote).toContainText("same guest");
  await expect(formNote).toContainText("hostname, SSH host keys, and machine ID");
  console.log(`form note: ${await formNote.innerText()}`);
  await page.screenshot({ path: "test-results/screens/host-clone-form.png", fullPage: true });

  await dialog.getByLabel("Name of the new VM").fill(cloneName);
  await dialog.getByRole("button", { name: "Clone" }).click();
  await expect(page.getByRole("heading", { name: cloneName, exact: true })).toBeVisible({ timeout: 30_000 });
  const pageNote = page.getByTestId("clone-identity-note");
  await expect(pageNote).toContainText("same guest");
  await expect(pageNote).toContainText("SSH host keys");
  console.log(`clone page: ${page.url()}`);
  console.log(`page note: ${await pageNote.innerText()}`);
  await page.screenshot({ path: "test-results/screens/host-clone-page.png", fullPage: true });

  // Delete the clone once its task has provisioned it.
  await expect(page.getByRole("button", { name: "Delete", exact: true })).toBeVisible({ timeout: 60_000 });
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete VM and disks" }).click();
  await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
  await expect(page.getByRole("link", { name: cloneName, exact: true })).toHaveCount(0, { timeout: 60_000 });
  expect(errors, "page errors").toEqual([]);
});
