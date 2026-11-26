/**
 * Settings test payments: input checks and how a test's progress reads.
 * No server or React imports, so it is unit tested.
 */

import { stkResult, type Tone } from "./mpesa";

export const TEST_MIN = 1;
export const TEST_MAX = 1000;

export type TestPaymentView = {
  charge_status: string;
  prompt_error: string | null;
  push: { status: string; result_code: string | null; result_desc: string | null } | null;
};

export type TestOutcome = {
  label: string;
  tone: Tone;
  /** No further change is expected without someone acting: stop polling. */
  final: boolean;
  detail: string | null;
};

/** Where a test payment stands, from its charge and its latest prompt. */
export function testOutcome(t: TestPaymentView): TestOutcome {
  if (t.charge_status === "succeeded") {
    return { label: "Paid", tone: "ok", final: true, detail: "Safaricom confirmed it with a receipt and it was recorded." };
  }
  if (t.charge_status === "needs_review") {
    return {
      label: "Needs review",
      tone: "warn",
      final: true,
      detail: "Safaricom said it was paid but sent no receipt. Confirm it on the transaction.",
    };
  }
  if (t.charge_status === "reversed") return { label: "Reversed", tone: "neutral", final: true, detail: null };
  if (!t.push) {
    if (t.prompt_error) return { label: "Prompt not sent", tone: "bad", final: true, detail: t.prompt_error };
    if (t.charge_status === "expired") return { label: "Expired", tone: "neutral", final: true, detail: null };
    return { label: "Starting", tone: "info", final: false, detail: null };
  }
  switch (t.push.status) {
    case "initiating":
      return { label: "Sending the prompt", tone: "info", final: false, detail: null };
    case "sent":
      return { label: "On the phone", tone: "violet", final: false, detail: "Enter the M-Pesa PIN on the phone to pay." };
    case "succeeded":
      // Prompt done; the charge update follows in the same callback.
      return { label: "Recording", tone: "info", final: false, detail: null };
    case "unknown":
      return {
        label: "No answer from Safaricom",
        tone: "warn",
        final: true,
        detail: "Staff will see it under M-Pesa callbacks or on the charge to confirm.",
      };
    case "failed":
      return { label: "Not paid", tone: "bad", final: true, detail: stkResult(t.push) ?? t.prompt_error ?? "The prompt failed." };
    default:
      return { label: t.push.status.replace(/_/g, " "), tone: "neutral", final: true, detail: null };
  }
}

/** Client-side check before posting; the server checks again. */
export function checkTestPayment(i: { amount: string; phone: string }): { field: "amount" | "phone"; message: string } | undefined {
  const amount = i.amount.trim();
  if (!/^\d+$/.test(amount) || Number(amount) < TEST_MIN || Number(amount) > TEST_MAX) {
    return { field: "amount", message: `Enter a whole number of shillings from ${TEST_MIN} to ${TEST_MAX.toLocaleString("en-KE")}.` };
  }
  const digits = i.phone.replace(/[\s-]/g, "");
  if (!/^(\+?254|0)(7|1)\d{8}$/.test(digits)) {
    return { field: "phone", message: "Enter a Safaricom number, e.g. 0712 345 678." };
  }
  return undefined;
}
