/**
 * M-Pesa (Daraja) helpers for the admin app: labels for payment prompts and
 * callbacks, and the forms staff use on payments (resolve, reverse, assign).
 * No server or React imports, so it is unit tested.
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
  app: "App API",
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

function reasonError(reason: string, example: string): string | undefined {
  const r = reason.trim();
  if (r.length < 3) return `Say briefly why, e.g. “${example}”.`;
  if (r.length > 500) return "Keep the reason under 500 characters.";
  return undefined;
}

// ---------------------------------------------------------------- reverse

export interface ReverseInput {
  reason: string;
  /** Safaricom's reversal receipt; required for M-Pesa payments. */
  receipt: string;
  receiptRequired: boolean;
  /** What staff typed to confirm. */
  confirm: string;
  /** What they must type: the payment's receipt, or REVERSE when it has none. */
  expected: string;
}

/** The text staff type to confirm a reversal of this payment. */
export function reverseConfirmWord(receipt: string | null): string {
  return receipt ? receipt.toUpperCase() : "REVERSE";
}

/** Field errors for the reverse form (empty when valid). Mirrors the admin API's rules. */
export function validateReverse(i: ReverseInput): Partial<Record<"reason" | "receipt" | "confirm", string>> {
  const errors: Partial<Record<"reason" | "receipt" | "confirm", string>> = {};
  const reason = reasonError(i.reason, "Customer’s bank recalled the payment, ticket 4411");
  if (reason) errors.reason = reason;
  const r = normalizeReceipt(i.receipt);
  if (i.receiptRequired && !r) errors.receipt = "Enter Safaricom’s receipt for the reversal.";
  else if (r && !RECEIPT.test(r)) errors.receipt = "A receipt is letters and digits only, like SKL4ABCD12.";
  if (i.confirm.trim().toUpperCase() !== i.expected.toUpperCase()) errors.confirm = `Type ${i.expected} to confirm.`;
  return errors;
}

// ----------------------------------------------------------------- assign

export interface AssignInput {
  appId: string;
  reason: string;
}

/** Field errors for the assign-to-app form (empty when valid). */
export function validateAssign(i: AssignInput): Partial<Record<"app_id" | "reason", string>> {
  const errors: Partial<Record<"app_id" | "reason", string>> = {};
  if (!/^[0-9a-f-]{36}$/i.test(i.appId.trim())) errors.app_id = "Choose the app this payment belongs to.";
  const reason = reasonError(i.reason, "Customer typed HSOT instead of HOST, confirmed by phone");
  if (reason) errors.reason = reason;
  return errors;
}

