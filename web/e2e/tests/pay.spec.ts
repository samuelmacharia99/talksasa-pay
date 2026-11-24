import { expect, test } from "@playwright/test";

const PAY = process.env.E2E_PAY_URL ?? "http://127.0.0.1:3000";
const OPEN = process.env.E2E_OPEN_TOKEN ?? "";
const PARTIAL = process.env.E2E_PARTIAL_TOKEN ?? "";
const PAID = process.env.E2E_PAID_TOKEN ?? "";
const SHOTS = "test-results/screens";

test.describe("hosted pay page", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("shows an open bill with paybill instructions", async ({ page }) => {
    test.skip(!OPEN, "E2E_OPEN_TOKEN not set (run via make e2e)");
    const res = await page.goto(`${PAY}/${OPEN}`);
    expect(res?.status()).toBe(200);
    expect(res?.headers()["content-security-policy"]).toContain("frame-ancestors 'none'");
    expect(res?.headers()["x-robots-tag"]).toContain("noindex");
    await expect(page.getByText("Amount due")).toBeVisible();
    await expect(page.getByText("174379")).toBeVisible();
    await expect(page.getByText("This page updates by itself")).toBeVisible();
    // Nothing personal on the page.
    await expect(page.locator("body")).not.toContainText(/@example\.com|2547\d{8}|07\d{8}/);
    await page.screenshot({ path: `${SHOTS}/20-pay-open.png`, fullPage: true });
  });

  test("shows what is left on a part-paid bill", async ({ page }) => {
    test.skip(!PARTIAL, "E2E_PARTIAL_TOKEN not set");
    await page.goto(`${PAY}/${PARTIAL}`);
    await expect(page.getByText("Left to pay")).toBeVisible();
    await expect(page.getByText(/already received/)).toBeVisible();
    await page.screenshot({ path: `${SHOTS}/21-pay-partial.png`, fullPage: true });
  });

  test("shows a paid bill as paid", async ({ page }) => {
    test.skip(!PAID, "E2E_PAID_TOKEN not set");
    await page.goto(`${PAY}/${PAID}`);
    await expect(page.getByText("Paid", { exact: true })).toBeVisible();
    await expect(page.getByText("Business number")).toHaveCount(0);
    await page.screenshot({ path: `${SHOTS}/22-pay-paid.png`, fullPage: true });
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
