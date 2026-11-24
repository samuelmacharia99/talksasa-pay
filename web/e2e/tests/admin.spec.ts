import { expect, test } from "@playwright/test";

import { nextStep, totp } from "./totp";

// Inputs prepared by `make e2e` (scripts/e2e.sh): a fresh admin invitation
// and two seeded payment links.
const ADMIN = process.env.E2E_ADMIN_URL ?? "http://127.0.0.1:3001";
const SETUP_URL = process.env.E2E_SETUP_URL ?? "";
const EMAIL = process.env.E2E_EMAIL ?? "";
const PASSWORD = "lantern-giraffe-copper-91"; // gitleaks:allow (throwaway E2E account password)
const SHOTS = "test-results/screens";

test.describe.configure({ mode: "serial" });

let totpSecret = "";
let recoveryCodes: string[] = [];

test("invited staff member sets up password and two-factor sign-in", async ({ page }) => {
  test.skip(!SETUP_URL, "E2E_SETUP_URL not set (run via make e2e)");
  const res = await page.goto(SETUP_URL);
  expect(res?.headers()["content-security-policy"]).toContain("nonce-");
  await expect(page.getByRole("heading", { name: /Welcome/ })).toBeVisible();
  await expect(page.getByAltText("QR code for your authenticator app")).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/01-setup.png`, fullPage: true });

  totpSecret = (await page.getByText(/^[A-Z2-7]{4}( [A-Z2-7]{1,4})+$/).innerText()).replace(/\s/g, "");
  expect(totpSecret).toHaveLength(32);

  // A mismatched confirmation is caught before anything is sent.
  await page.getByLabel("Password", { exact: true }).fill(PASSWORD);
  await page.getByLabel("Repeat password").fill(PASSWORD + "x");
  await page.getByLabel("Code from the app").fill(totp(totpSecret));
  await page.getByRole("button", { name: "Activate my account" }).click();
  await expect(page.locator("form [role=alert]")).toContainText("don’t match");

  // A wrong code is rejected by the server and nothing typed is lost.
  await page.getByLabel("Repeat password").fill(PASSWORD);
  await page.getByLabel("Code from the app").fill(totp(totpSecret, Date.now() - 5 * 60_000));
  await page.getByRole("button", { name: "Activate my account" }).click();
  await expect(page.locator("form [role=alert]")).toContainText("code is not valid");
  await expect(page.getByLabel("Password", { exact: true })).toHaveValue(PASSWORD);

  await page.getByLabel("Code from the app").fill(totp(totpSecret));
  await page.getByRole("button", { name: "Activate my account" }).click();
  await expect(page.getByRole("heading", { name: "Save your recovery codes" })).toBeVisible();
  recoveryCodes = await page.locator("ol li").allInnerTexts();
  expect(recoveryCodes).toHaveLength(10);
  await page.screenshot({ path: `${SHOTS}/02-recovery-codes.png`, fullPage: true });

  // The link is now spent.
  await page.goto(SETUP_URL);
  await expect(page.getByRole("heading", { name: "This setup link can’t be used" })).toBeVisible();
});

test("signs in, browses every section, manages a key, signs out", async ({ page, context }) => {
  test.skip(!totpSecret, "setup did not run");

  // Wrong password: generic message.
  await page.goto(`${ADMIN}/login`);
  await page.screenshot({ path: `${SHOTS}/03-login.png`, fullPage: true });
  await page.getByLabel("Email").fill(EMAIL);
  await page.getByLabel("Password").fill("definitely-wrong-password");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.locator("form [role=alert]")).toContainText("Sign-in failed");

  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page).toHaveURL(/\/login\/verify$/);
  await nextStep(); // the setup code's time step was consumed
  await page.getByLabel("Authentication code").fill(totp(totpSecret));
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: /collected today/ })).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/04-overview.png`, fullPage: true });

  // The session cookie is out of reach of page scripts.
  const cookies = await context.cookies();
  const session = cookies.find((c) => c.name.endsWith("tsp_admin"));
  expect(session?.httpOnly).toBe(true);
  expect(session?.sameSite).toBe("Strict");
  expect(await page.evaluate(() => document.cookie)).not.toContain("tsp_admin");

  await page.getByRole("link", { name: "Transactions", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Transactions" })).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/05-transactions.png`, fullPage: true });
  await page.locator("tbody tr").first().getByRole("link").first().click();
  await expect(page.getByRole("heading", { name: "Where the money went" })).toBeVisible();
  await expect(page.locator("body")).not.toContainText(/2547\d{8}/); // phones are masked
  await page.screenshot({ path: `${SHOTS}/06-transaction.png`, fullPage: true });

  await page.getByRole("link", { name: "Unmatched payments" }).click();
  await expect(page.getByText("SDSUSPENSE01").first()).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/07-unmatched.png`, fullPage: true });

  await page.getByRole("link", { name: "Payers" }).click();
  await page.getByLabel("Search").fill("bill 1007");
  await page.getByRole("button", { name: "Apply filters" }).click();
  await expect(page.getByRole("link", { name: "BILL-1007" })).toBeVisible();
  await page.getByRole("link", { name: "BILL-1007" }).click();
  await expect(page.getByRole("heading", { name: "Kilimani Fibre Ltd" })).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/08-payer.png`, fullPage: true });

  await page.getByRole("link", { name: "Payment requests" }).click();
  await page.locator("select[name=status]").selectOption("partially_paid");
  await page.getByRole("button", { name: "Apply filters" }).click();
  await expect(page.locator("tbody tr").first()).toContainText("Part paid");
  await page.locator("tbody tr").first().getByRole("link").first().click();
  await expect(page.getByText("Paid so far")).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/09-request.png`, fullPage: true });

  await page.getByRole("link", { name: "Products" }).click();
  await page.getByRole("link", { name: "Cloud Hosting" }).click();
  await page.getByLabel("Label").fill(`e2e ${Date.now()}`);
  await page.getByRole("button", { name: "Issue key" }).click();
  const secret = page.locator("code");
  await expect(secret).toContainText(/^tsp_test_[a-z2-7]+_[a-z2-7]+$/);
  await page.screenshot({ path: `${SHOTS}/10-key-issued.png`, fullPage: true });
  await page.getByRole("button", { name: "I have stored it safely" }).click();
  await page.reload();
  await expect(page.locator("body")).not.toContainText("tsp_test_");
  await page.getByRole("button", { name: "Revoke" }).first().click();
  await page.getByRole("button", { name: "Revoke now" }).click();
  await expect(page.getByText("Revoked").first()).toBeVisible();

  await page.getByRole("link", { name: "Audit log" }).click();
  await expect(page.getByText("Chain verified intact")).toBeVisible();
  await expect(page.getByText("api_key.revoked").first()).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/11-audit.png`, fullPage: true });

  await page.getByRole("link", { name: "Staff" }).click();
  await expect(page.getByText(EMAIL)).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/12-staff.png`, fullPage: true });

  // Developers: the product API guides and the generated reference.
  await page.getByRole("link", { name: "Developers" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Talksasa Pay product API" })).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/13-developers.png`, fullPage: true });
  await page.getByRole("navigation", { name: "Developer guides" }).getByRole("link", { name: "M-Pesa prompts" }).click();
  await expect(page).toHaveURL(/\/developers\/stk-push$/);
  await expect(page.getByRole("button", { name: "Copy" }).first()).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/14-developers-guide.png`, fullPage: true });
  await page.getByRole("navigation", { name: "Developer guides" }).getByRole("link", { name: "API reference" }).click();
  await expect(page.locator("#sendSTKPush")).toContainText("/v1/payment-requests/{id}/stk-push");
  await page.screenshot({ path: `${SHOTS}/15-developers-reference.png`, fullPage: true });
  const spec = await page.request.get(`${ADMIN}/developers/openapi.yaml`);
  expect(spec.ok()).toBe(true);
  expect(await spec.text()).toMatch(/^openapi: 3\./);

  // Sign out, then prove the old cookie is dead server-side.
  const before = (await context.cookies()).find((c) => c.name.endsWith("tsp_admin"))!;
  await page.getByRole("button", { name: "Sign out" }).first().click();
  await expect(page).toHaveURL(/\/login/);
  await context.addCookies([before]);
  await page.goto(`${ADMIN}/`);
  await expect(page).toHaveURL(/\/login\?ended=1/);
});

test("signs in with a recovery code exactly once", async ({ page }) => {
  test.skip(recoveryCodes.length === 0, "setup did not run");
  for (const expectOk of [true, false]) {
    await page.goto(`${ADMIN}/login`);
    await page.getByLabel("Email").fill(EMAIL);
    await page.getByLabel("Password").fill(PASSWORD);
    await page.getByRole("button", { name: "Continue" }).click();
    await page.getByLabel("Authentication code").fill(recoveryCodes[0]!);
    await page.getByRole("button", { name: "Sign in" }).click();
    if (expectOk) {
      await expect(page.getByRole("heading", { name: /collected today/ })).toBeVisible();
      await page.getByRole("button", { name: "Sign out" }).first().click();
    } else {
      await expect(page.locator("form [role=alert]")).toContainText("not valid");
    }
  }
});

test("pages without a session go to sign-in", async ({ page }) => {
  for (const path of ["/", "/transactions", "/staff", "/products"]) {
    await page.goto(`${ADMIN}${path}`);
    await expect(page).toHaveURL(/\/login/);
  }
});
