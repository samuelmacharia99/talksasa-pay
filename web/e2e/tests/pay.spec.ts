import { expect, test } from "@playwright/test";

/*
 * The hosted checkout page, one per charge. Inputs from `make e2e`
 * (scripts/e2e.sh), each a charge's hosted token:
 *   E2E_PENDING_TOKEN  a pending charge (payable)
 *   E2E_SUCCEEDED_TOKEN  a succeeded charge (alias E2E_PAID_TOKEN)
 *   E2E_EXPIRED_TOKEN    an expired or cancelled charge; may be empty
 */
const PAY = process.env.E2E_PAY_URL ?? "http://127.0.0.1:3000";
const PENDING = process.env.E2E_PENDING_TOKEN || process.env.E2E_OPEN_TOKEN || "";
const PAID = process.env.E2E_SUCCEEDED_TOKEN || process.env.E2E_PAID_TOKEN || "";
const CLOSED = process.env.E2E_EXPIRED_TOKEN || process.env.E2E_CLOSED_TOKEN || "";
const SHOTS = "test-results/screens";

test.describe("checkout page", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("shows a pending charge with the paybill steps", async ({ page }) => {
    test.skip(!PENDING, "E2E_PENDING_TOKEN not set (run via make e2e)");
    const res = await page.goto(`${PAY}/${PENDING}`);
    expect(res?.status()).toBe(200);
    expect(res?.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
    expect(res?.headers()["x-robots-tag"]).toContain("noindex");
    await expect(page.getByText("Amount to pay")).toBeVisible();
    await expect(page.getByText("Business number", { exact: true })).toBeVisible();
    await expect(page.getByText("Account number", { exact: true })).toBeVisible();
    await expect(page.getByText("This page updates by itself")).toBeVisible();
    // Nothing personal, and no billing-ledger wording ("Pay Bill" is the M-Pesa menu).
    await expect(page.locator("body")).not.toContainText(/@example\.com|2547\d{8}|07\d{8}/);
    await expect(page.locator("body")).not.toContainText(/\bpayer\b|\bcredit\b|Left to pay|already received/i);
    await page.screenshot({ path: `${SHOTS}/20-pay-pending.png`, fullPage: true });
  });

  test("shows a succeeded charge as paid, with a receipt slip", async ({ page }) => {
    test.skip(!PAID, "E2E_SUCCEEDED_TOKEN not set");
    await page.goto(`${PAY}/${PAID}`);
    await expect(page.getByText("Paid", { exact: true })).toBeVisible();
    await expect(page.getByText("Paid to")).toBeVisible();
    await expect(page.getByText("Business number")).toHaveCount(0);
    await page.screenshot({ path: `${SHOTS}/21-pay-paid.png`, fullPage: true });
  });

  test("shows a closed charge as no longer payable", async ({ page }) => {
    test.skip(!CLOSED, "E2E_EXPIRED_TOKEN not set");
    await page.goto(`${PAY}/${CLOSED}`);
    await expect(page.getByText(/This payment (was cancelled|link has expired)/)).toBeVisible();
    await expect(page.getByText("Business number")).toHaveCount(0);
    await page.screenshot({ path: `${SHOTS}/22-pay-closed.png`, fullPage: true });
  });

  test("rejects unknown and malformed links the same way", async ({ page }) => {
    for (const t of ["a".repeat(52), "nope", "../../admin"]) {
      const res = await page.goto(`${PAY}/${t}`);
      expect(res?.status()).toBe(404);
      await expect(page.getByRole("heading", { name: "This payment link isn’t valid" })).toBeVisible();
    }
    await page.screenshot({ path: `${SHOTS}/23-pay-invalid.png`, fullPage: true });
  });
});
