import { expect, test } from "@playwright/test";

/*
 * M-Pesa payment prompt (STK push) on the hosted pay page, against the dev
 * stack with the fake Daraja (TSP_MPESA_* pointing at the fake-daraja
 * service). The fake answers by the phone's last digit:
 *   ...0  succeeds (with a receipt) after about 300 ms
 *   ...1  is cancelled by the customer (ResultCode 1032)
 *
 * Needs two different open bills with a balance of at most KES 250,000:
 *   E2E_STK_TOKEN         paid by the happy path (the bill ends up paid)
 *   E2E_STK_CANCEL_TOKEN  used for the cancel path (stays open)
 */
const PAY = process.env.E2E_PAY_URL ?? "http://127.0.0.1:3000";
const STK = process.env.E2E_STK_TOKEN ?? "";
const STK_CANCEL = process.env.E2E_STK_CANCEL_TOKEN ?? "";
const SHOTS = "test-results/screens";

test.describe("pay page M-Pesa prompt", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("sends a prompt and shows the bill as paid", async ({ page }) => {
    test.skip(!STK, "E2E_STK_TOKEN not set (run via make e2e with the fake Daraja)");
    const res = await page.goto(`${PAY}/${STK}`);
    expect(res?.status()).toBe(200);

    // The browser only talks to the pay app itself.
    const csp = res?.headers()["content-security-policy"] ?? "";
    expect(csp).toMatch(/connect-src 'self'/);
    const outside: string[] = [];
    page.on("request", (r) => {
      if (!r.url().startsWith(PAY)) outside.push(r.url());
    });

    await expect(page.getByRole("heading", { name: "Pay from your phone" })).toBeVisible();
    await expect(page.getByText("Or pay with Paybill")).toBeVisible();
    const phone = page.getByLabel("M-Pesa phone number");
    await expect(phone).toHaveAttribute("inputmode", "tel");
    await expect(phone).toHaveAttribute("autocomplete", "tel");

    // Client-side validation first.
    await phone.fill("12345");
    await page.getByRole("button", { name: "Send payment prompt" }).click();
    await expect(page.getByText("Enter a Safaricom number, like 0712 345 678.")).toBeVisible();
    await expect(phone).toHaveAttribute("aria-invalid", "true");

    await phone.fill("0712 345 670");
    await page.screenshot({ path: `${SHOTS}/30-pay-stk-form.png`, fullPage: true });
    const sent = page.waitForResponse((r) => r.url().includes(`/api/stk/${STK}`) && r.request().method() === "POST");
    await page.getByRole("button", { name: "Send payment prompt" }).click();
    expect((await sent).status()).toBe(202);

    await expect(page.getByText("Check your phone and enter your M-Pesa PIN")).toBeVisible();
    await page.screenshot({ path: `${SHOTS}/31-pay-stk-waiting.png`, fullPage: true });

    // The fake confirms in ~300 ms; the callback is processed by the worker,
    // then polling refreshes the page into the paid state.
    await expect(page.getByText("Paid", { exact: true })).toBeVisible({ timeout: 45_000 });
    await expect(page.getByText("Business number")).toHaveCount(0);
    await page.screenshot({ path: `${SHOTS}/32-pay-stk-paid.png`, fullPage: true });

    // The phone number is never echoed back onto the page.
    await expect(page.locator("body")).not.toContainText(/2547\d{8}|07\d{8}|0712 345 670/);
    expect(outside).toEqual([]);
  });

  test("shows the reason and offers a retry when the customer cancels", async ({ page }) => {
    test.skip(!STK_CANCEL, "E2E_STK_CANCEL_TOKEN not set (run via make e2e with the fake Daraja)");
    await page.goto(`${PAY}/${STK_CANCEL}`);

    await page.getByLabel("M-Pesa phone number").fill("+254712345671");
    await page.getByRole("button", { name: "Send payment prompt" }).click();
    await expect(page.getByText("Check your phone and enter your M-Pesa PIN")).toBeVisible();

    await expect(page.getByText("The payment didn’t go through")).toBeVisible({ timeout: 45_000 });
    await expect(page.getByRole("button", { name: "Try again" })).toBeVisible();
    // The paybill steps stay available as the fallback.
    await expect(page.getByText("Business number")).toBeVisible();
    await page.screenshot({ path: `${SHOTS}/33-pay-stk-cancelled.png`, fullPage: true });

    await page.getByRole("button", { name: "Try again" }).click();
    await expect(page.getByLabel("M-Pesa phone number")).toBeFocused();
    await expect(page.getByRole("button", { name: "Send payment prompt" })).toBeVisible();
  });

  test("the prompt endpoint refuses cross-site and non-JSON posts", async ({ request }) => {
    test.skip(!STK_CANCEL, "E2E_STK_CANCEL_TOKEN not set");
    const form = await request.post(`${PAY}/api/stk/${STK_CANCEL}`, { form: { phone: "0712345671" } });
    expect(form.status()).toBe(415);
    const cross = await request.post(`${PAY}/api/stk/${STK_CANCEL}`, {
      data: { phone: "0712345671" },
      headers: { "Sec-Fetch-Site": "cross-site" },
    });
    expect(cross.status()).toBe(403);
    const bad = await request.post(`${PAY}/api/stk/${STK_CANCEL}`, { data: { phone: "0812345678" } });
    expect(bad.status()).toBe(422);
  });
});
