import { chromium, expect, test } from "@playwright/test";

// The trust controls of ADR 0010 amendment x: the suite reaches skep-api
// only because Chromium is pinned to its key. Without the pin, or with
// another key's pin, Chromium refuses the certificate; with the pin from
// the configuration it loads. (The Node-side request client trusts only
// the certificate in NODE_EXTRA_CA_CERTS, which scripts/web-e2e.sh writes
// after comparing it with the pin.)
test("Chromium trusts skep-api only by its key pin", async ({ page, request, baseURL }) => {
  const health = `${baseURL}/api/v1/health`;
  const otherKey = Buffer.alloc(32).toString("base64");
  for (const args of [[], [`--ignore-certificate-errors-spki-list=${otherKey}`]]) {
    const browser = await chromium.launch({ args });
    const other = await browser.newPage();
    await expect(other.goto(health), `launched with ${JSON.stringify(args)}`).rejects.toThrow(/ERR_CERT_/);
    await browser.close();
  }
  const res = await page.goto(health);
  expect(res?.status()).toBe(200);
  expect((await request.get(health)).status()).toBe(200);
});
