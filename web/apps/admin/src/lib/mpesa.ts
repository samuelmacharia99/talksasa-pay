/**
 * M-Pesa (Daraja) helpers for the admin app: labels for payment prompts and
 * callbacks, the needs-review resolve form's validation, and the overview's
 * M-Pesa attention items. No server or React imports, so it is unit tested.
 */

export type Tone = "ok" | "warn" | "bad" | "info" | "neutral" | "violet";

export const STK_STATUS: Record<string, { label: string; tone: Tone }> = {
  initiating: { label: "Sending", tone: "info" },
  sent: { label: "On the phone", tone: "violet" },
  succeeded: { label: "Paid", tone: "ok" },
  failed: { label: "Failed", tone: "bad" },
  unknown: { label: "No answer", tone: "warn" },
  needs_review: { label: "Needs review", tone: "bad" },
};

export function stkStatus(s: string): { label: string; tone: Tone } {
  return STK_STATUS[s] ?? { label: s.replace(/_/g, " "), tone: "neutral" };
}

export const STK_SOURCE: Record<string, string> = {
  product: "Product API",
  pay_page: "Pay page",
};

export const CALLBACK_KIND: Record<string, string> = {
  stk: "Payment prompt result",
  c2b_confirmation: "Paybill payment",
};

/** A short result for a prompt row: Safaricom's description, with the code. */
export function stkResult(p: { result_code: string | null; result_desc: string | null }): string | null {
  if (!p.result_desc && !p.result_code) return null;
  if (p.result_desc && p.result_code) return `${p.result_desc} (code ${p.result_code})`;
  return p.result_desc ?? `Code ${p.result_code}`;
}

export type CallbackState = { label: string; tone: Tone };

/** Processed, waiting for the worker, or failing (has an error). */
export function callbackState(cb: { processed_at: string | null; error: string | null }): CallbackState {
  if (cb.processed_at) return { label: "Processed", tone: "ok" };
  if (cb.error) return { label: "Failing", tone: "bad" };
  return { label: "Waiting", tone: "info" };
}

// ---------------------------------------------------------------- resolve

export type ResolveAction = "confirm" | "fail";

/** M-Pesa receipts are 10 letters and digits (e.g. SKL4ABCD12); allow a little slack for older formats. */
const RECEIPT = /^[A-Z0-9]{8,20}$/;

export function normalizeReceipt(s: string): string {
  return s.trim().toUpperCase().replace(/\s+/g, "");
}

export interface ResolveInput {
  action: ResolveAction;
  receipt: string;
  reason: string;
}

/** Field errors for the resolve form (empty when valid). Mirrors the admin API's rules. */
export function validateResolve(i: ResolveInput): Partial<Record<"receipt" | "reason" | "action", string>> {
  const errors: Partial<Record<"receipt" | "reason" | "action", string>> = {};
  if (i.action !== "confirm" && i.action !== "fail") errors.action = "Choose confirm or not paid.";
  if (i.action === "confirm") {
    const r = normalizeReceipt(i.receipt);
    if (!r) errors.receipt = "Enter the M-Pesa receipt from the M-Pesa portal.";
    else if (!RECEIPT.test(r)) errors.receipt = "A receipt is letters and digits only, like SKL4ABCD12.";
  }
  const reason = i.reason.trim();
  if (reason.length < 3) errors.reason = "Say briefly how you checked, e.g. “Found on the M-Pesa portal at 10:42”.";
  else if (reason.length > 500) errors.reason = "Keep the reason under 500 characters.";
  return errors;
}

// --------------------------------------------------------------- overview

/**
 * Counts the overview may get from GET /dashboard once the backend reports
 * them. Optional so the app compiles whether or not the generated types
 * have them yet.
 */
export interface MpesaCounts {
  unknown_stk_pushes?: number;
  unprocessed_callbacks?: number;
  inactive_product_payments?: number;
}

export interface AttentionItem {
  href: string;
  label: string;
  detail: string;
  urgent: boolean;
}

const plural = (n: number, one: string, many: string) => (n === 1 ? `1 ${one}` : `${n} ${many}`);

/** Overview "Needs attention" items for M-Pesa; only counts that are present and non-zero. */
export function mpesaAttention(c: MpesaCounts): AttentionItem[] {
  const out: AttentionItem[] = [];
  if (typeof c.unknown_stk_pushes === "number" && c.unknown_stk_pushes > 0) {
    out.push({
      href: "/transactions?status=needs_review&channel=mpesa_stk",
      label: plural(c.unknown_stk_pushes, "payment prompt has no answer", "payment prompts have no answer"),
      detail: "Safaricom said paid but sent no receipt; find each payment on the M-Pesa portal and resolve it",
      urgent: true,
    });
  }
  if (typeof c.unprocessed_callbacks === "number" && c.unprocessed_callbacks > 0) {
    out.push({
      href: "/mpesa/callbacks?unprocessed=true",
      label: plural(c.unprocessed_callbacks, "M-Pesa callback not processed", "M-Pesa callbacks not processed"),
      detail: "Messages from Safaricom are stored but not yet applied; check the error column",
      urgent: true,
    });
  }
  if (typeof c.inactive_product_payments === "number" && c.inactive_product_payments > 0) {
    out.push({
      href: "/transactions",
      label: plural(c.inactive_product_payments, "payment for an inactive product or payer", "payments for inactive products or payers"),
      detail: "Applied to their bills as usual; tell the product team the customer is still paying",
      urgent: false,
    });
  }
  return out;
}
