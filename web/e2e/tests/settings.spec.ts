import { expect, test } from "@playwright/test";

import { nextStep, totp } from "./totp";

// The M-Pesa paybill Settings page against the fake Daraja (make e2e).
// Inputs from scripts/e2e.sh: a fresh admin invitation of its own, and the
// fake Daraja's throwaway development credentials (it only accepts those).
const ADMIN = process.env.E2E_ADMIN_URL ?? "http://127.0.0.1:3001";
const SETUP_URL = process.env.E2E_SETTINGS_SETUP_URL ?? "";
const EMAIL = process.env.E2E_SETTINGS_EMAIL ?? "";
const KEY = process.env.E2E_MPESA_CONSUMER_KEY ?? "";
const SECRET = process.env.E2E_MPESA_CONSUMER_SECRET ?? "";
const PASSKEY = process.env.E2E_MPESA_PASSKEY ?? "";
const PASSWORD = "velvet-otter-harbour-47"; // gitleaks:allow (throwaway E2E account password)
const SHOTS = "test-results/screens";

test("admin saves sandbox paybill settings, tests them and registers the callback URLs", async ({ page }) => {
  test.skip(!SETUP_URL || !KEY, "E2E_SETTINGS_SETUP_URL / E2E_MPESA_* not set (run via make e2e)");
  test.setTimeout(150_000);

  // A fresh admin: set up, then sign in.
  await page.goto(SETUP_URL);
  const secret = (await page.getByText(/^[A-Z2-7]{4}( [A-Z2-7]{1,4})+$/).innerText()).replace(/\s/g, "");
  await page.getByLabel("Password", { exact: true }).fill(PASSWORD);
  await page.getByLabel("Repeat password").fill(PASSWORD);
  await page.getByLabel("Code from the app").fill(totp(secret));
  await page.getByRole("button", { name: "Activate my account" }).click();
  await expect(page.getByRole("heading", { name: "Save your recovery codes" })).toBeVisible();
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel("Email").fill(EMAIL);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page).toHaveURL(/\/login\/verify$/);
  await nextStep();
  await page.getByLabel("Authentication code").fill(totp(secret));
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).not.toHaveURL(/\/login/);

  // Settings (linked from the nav's Admin group).
  await page.goto(`${ADMIN}/settings`);
  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");

  // Save sandbox settings pointing at the fake Daraja's credentials.
  await page.getByLabel("Environment").selectOption("sandbox");
  await page.getByLabel("Paybill number").fill("174379");
  await page.getByLabel("STK shortcode (optional)").fill("");
  // The callback base URL is not typed in: here the deployment fixes it.
  await expect(page.getByTestId("callback-base")).toHaveText("http://api:8088/");
  await expect(page.getByText("Set by the deployment configuration (TSP_MPESA_CALLBACK_BASE_URL).")).toBeVisible();
  await page.getByLabel("Consumer key").fill(KEY);
  await page.getByLabel("Consumer secret").fill(SECRET);
  await page.getByLabel("Passkey").fill(PASSKEY);
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Settings saved.")).toBeVisible();

  // Write-only: the values are gone from the form and the page; at most
  // the last four characters are shown.
  await expect(page.getByLabel("Consumer key")).toHaveValue("");
  await expect(page.getByText(`Saved ••••${KEY.slice(-4)}`)).toBeVisible();
  const html = await page.content();
  for (const v of [KEY, SECRET, PASSKEY]) expect(html).not.toContain(v);
  await expect(page.getByText("Saved here")).toBeVisible();

  // Test the credentials against the fake Daraja.
  await page.getByRole("button", { name: "Test credentials" }).click();
  await expect(page.getByTestId("test-result")).toContainText("Credentials accepted");

  // Register the C2B URLs; the registration time appears.
  await page.getByRole("button", { name: "Register URLs with Safaricom" }).click();
  await expect(page.getByText(/Safaricom accepted .*\/cb\/hooks\/•+\/c2b\/confirmation/)).toBeVisible();
  await page.reload();
  await expect(page.getByTestId("registration")).toContainText(/\d{2}:\d{2}/);
  await expect(page.getByTestId("registration")).not.toContainText("older paybill");
  expect(await page.content()).not.toContain("/cb/hooks/" + (process.env.E2E_MPESA_CALLBACK_TOKEN ?? "never-set"));
  await page.screenshot({ path: `${SHOTS}/settings.png`, fullPage: true });

  // Test payments: the fake Daraja pays phones ending in 0 and cancels
  // phones ending in 1. Each runs the real charge + prompt + callback path.
  const tests = page.getByRole("form", { name: "Test payment" });
  const before = await page.getByTestId("test-payment").count();
  await tests.getByLabel("Amount (KES)").fill("1001");
  await tests.getByLabel("Phone number").fill("0712345670");
  await tests.getByRole("button", { name: "Send test payment" }).click();
  // Over the cap: the browser refuses to submit, so no prompt is sent.
  expect(await tests.getByLabel("Amount (KES)").evaluate((el) => (el as HTMLInputElement).validity.rangeOverflow)).toBe(true);
  await expect(page.getByTestId("test-payment")).toHaveCount(before);

  await tests.getByLabel("Amount (KES)").fill("3");
  await tests.getByRole("button", { name: "Send test payment" }).click();
  const paid = page.getByTestId("test-payment").first();
  await expect(paid).toContainText("KES 3");
  await expect(paid).toContainText("Paid", { timeout: 60_000 });

  await tests.getByLabel("Amount (KES)").fill("2");
  await tests.getByLabel("Phone number").fill("0712345671");
  await tests.getByRole("button", { name: "Send test payment" }).click();
  const cancelled = page.getByTestId("test-payment").first();
  await expect(cancelled).toContainText("KES 2");
  await expect(cancelled).toContainText("Not paid", { timeout: 60_000 });
  await page.screenshot({ path: `${SHOTS}/settings-test-payments.png`, fullPage: true });

  // The money sits on the internal Gateway test app, marked as such.
  await paid.getByRole("link", { name: "Charge" }).click();
  await expect(page.getByText("Gateway test").first()).toBeVisible();
  await page.goto(`${ADMIN}/apps`);
  await expect(page.getByRole("row", { name: /Gateway test/ })).toContainText("Test");
});
