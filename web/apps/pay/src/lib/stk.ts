/**
 * M-Pesa payment prompt (STK push) logic for the pay page, kept free of
 * React and server imports so it is unit tested.
 */

export type StkStatus = "initiating" | "sent" | "succeeded" | "failed" | "needs_review";

/** The latest prompt as the page sees it (no ids, no phone number). */
export interface StkSnapshot {
  status: StkStatus;
  message: string | null;
}

/**
 * Normalises a Kenyan mobile number typed in any common way (0712 345 678,
 * 712345678, 254712345678, +254 712 345 678, 0112...) to the local form
 * 07XXXXXXXX / 01XXXXXXXX. Returns null when it is not a Kenyan mobile.
 */
export function normalizeKenyanPhone(input: string): string | null {
  if (typeof input !== "string" || input.length > 32) return null;
  const compact = input.trim().replace(/[\s\-().]/g, "");
  const m = /^(?:\+?254|0)?([17]\d{8})$/.exec(compact);
  return m ? `0${m[1]}` : null;
}

export const PHONE_HINT = "The Safaricom number that will pay, like 0712 345 678.";
export const PHONE_ERROR = "Enter a Safaricom number, like 0712 345 678.";

/** What the prompt section shows. */
export type StkView =
  | { kind: "form" }
  | { kind: "waiting"; title: string; body: string }
  | { kind: "failed"; title: string; body: string }
  | { kind: "confirming"; title: string; body: string }
  | { kind: "received"; title: string; body: string };

/** Maps the latest prompt to what the customer sees. */
export function stkView(stk: StkSnapshot | null): StkView {
  if (!stk) return { kind: "form" };
  switch (stk.status) {
    case "initiating":
    case "sent":
      return {
        kind: "waiting",
        title: "Check your phone and enter your M-Pesa PIN",
        body: "We sent a payment prompt to your phone. It can take a few seconds to appear. Keep this page open.",
      };
    case "failed":
      return {
        kind: "failed",
        title: "The payment didn’t go through",
        body: stk.message?.trim() || "The payment prompt was not completed. No money was taken.",
      };
    case "needs_review":
      return {
        kind: "confirming",
        title: "We’re confirming your payment with Safaricom",
        body: "This can take a few minutes. Please don’t pay again. This page updates by itself.",
      };
    case "succeeded":
      return {
        kind: "received",
        title: "Payment received",
        body: "Thank you. Updating your bill…",
      };
    default:
      return { kind: "form" };
  }
}

/** Whether the page should poll faster: a prompt is on its way or being confirmed. */
export function stkActive(stk: StkSnapshot | null): boolean {
  return !!stk && (stk.status === "initiating" || stk.status === "sent" || stk.status === "needs_review" || stk.status === "succeeded");
}

/**
 * The outcome of asking for a prompt, as the browser sees our route's
 * answer. `fallback` means the paybill steps are the way to pay now, so the
 * form is hidden.
 */
export interface SendOutcome {
  /** A prompt is (now) live: show the waiting state. */
  waiting: boolean;
  /** Message for the customer, if any. */
  error?: string;
  /** The error is about the phone field. */
  fieldError?: boolean;
  /** Prompts can't be used for this bill now; point to the paybill steps. */
  fallback?: boolean;
  /** The bill changed (e.g. paid or closed): reload the page. */
  refresh?: boolean;
}

/** Maps an error answer of POST /api/stk/{token} (status + Go problem code) to what to show. */
export function sendErrorOutcome(status: number, code: string | undefined): SendOutcome {
  switch (true) {
    case code === "stk_in_progress":
      return { waiting: true };
    case code === "amount_too_large":
      return {
        waiting: false,
        fallback: true,
        error: "This amount is too large for an M-Pesa prompt. Pay with the paybill steps below.",
      };
    case code === "not_payable":
      return { waiting: false, refresh: true, error: "This bill can’t be paid any more." };
    case status === 422 || code === "validation_failed" || code === "invalid_phone":
      return { waiting: false, fieldError: true, error: PHONE_ERROR };
    case status === 429:
      return {
        waiting: false,
        error: "Too many attempts. Wait a minute and try again, or pay with the paybill steps below.",
      };
    case status === 503 || code === "mpesa_unavailable":
      return {
        waiting: false,
        fallback: true,
        error: "Payment prompts aren’t available right now. Pay with the paybill steps below.",
      };
    case status === 502 || code === "mpesa_error":
      return {
        waiting: false,
        error: "M-Pesa couldn’t send the prompt. Try again in a moment, or pay with the paybill steps below.",
      };
    case status === 404:
      return { waiting: false, refresh: true, error: "This payment link isn’t valid any more." };
    default:
      return {
        waiting: false,
        error: "Something went wrong. Try again, or pay with the paybill steps below.",
      };
  }
}
