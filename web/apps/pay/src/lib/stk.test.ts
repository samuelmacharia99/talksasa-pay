import { describe, expect, it } from "vitest";

import { normalizeKenyanPhone, sendErrorOutcome, stkActive, stkView } from "./stk";

describe("normalizeKenyanPhone", () => {
  it.each([
    ["0712345678", "0712345678"],
    ["0712 345 678", "0712345678"],
    ["  0712-345-678 ", "0712345678"],
    ["712345678", "0712345678"],
    ["254712345678", "0712345678"],
    ["+254712345678", "0712345678"],
    ["+254 (712) 345 678", "0712345678"],
    ["0112345678", "0112345678"],
    ["+254112345678", "0112345678"],
  ])("accepts %s", (input, want) => {
    expect(normalizeKenyanPhone(input)).toBe(want);
  });

  it.each([
    "",
    "071234567",
    "07123456789",
    "0812345678",
    "+255712345678",
    "25407123456789",
    "07123456a8",
    "phone",
    "0".repeat(40),
  ])("rejects %s", (input) => {
    expect(normalizeKenyanPhone(input)).toBeNull();
  });
});

describe("stkView", () => {
  it("shows the form when there is no prompt", () => {
    expect(stkView(null)).toEqual({ kind: "form" });
  });

  it("asks the customer to check their phone while the prompt is live", () => {
    for (const status of ["initiating", "sent"] as const) {
      const v = stkView({ status, message: null });
      expect(v.kind).toBe("waiting");
      if (v.kind === "waiting") expect(v.title).toBe("Check your phone and enter your M-Pesa PIN");
    }
  });

  it("shows Safaricom's reason when the prompt failed", () => {
    const v = stkView({ status: "failed", message: "You cancelled the prompt" });
    expect(v).toMatchObject({ kind: "failed", body: "You cancelled the prompt" });
    expect(stkView({ status: "failed", message: null })).toMatchObject({ kind: "failed", body: expect.stringContaining("No money") });
  });

  it("tells the customer not to pay again while Safaricom confirms", () => {
    const v = stkView({ status: "needs_review", message: null });
    expect(v.kind).toBe("confirming");
    if (v.kind === "confirming") {
      expect(v.title).toBe("We’re confirming your payment with Safaricom");
      expect(v.body).toContain("don’t pay again");
    }
  });

  it("says received on success (the bill itself flips to paid on refresh)", () => {
    expect(stkView({ status: "succeeded", message: null }).kind).toBe("received");
  });

  it("polls faster only while something is happening", () => {
    expect(stkActive(null)).toBe(false);
    expect(stkActive({ status: "sent", message: null })).toBe(true);
    expect(stkActive({ status: "needs_review", message: null })).toBe(true);
    expect(stkActive({ status: "failed", message: "x" })).toBe(false);
  });
});

describe("sendErrorOutcome", () => {
  it("treats a live prompt as waiting, not an error", () => {
    expect(sendErrorOutcome(409, "stk_in_progress")).toEqual({ waiting: true });
  });

  it("falls back to the paybill when prompts can't be used", () => {
    expect(sendErrorOutcome(409, "amount_too_large")).toMatchObject({ fallback: true });
    expect(sendErrorOutcome(503, "mpesa_unavailable")).toMatchObject({ fallback: true });
    expect(sendErrorOutcome(503, undefined)).toMatchObject({ fallback: true });
  });

  it("marks phone errors as field errors", () => {
    expect(sendErrorOutcome(422, "validation_failed")).toMatchObject({ fieldError: true });
  });

  it("keeps the form for retryable problems", () => {
    for (const [s, c] of [[429, "rate_limited"], [502, "mpesa_error"], [500, undefined]] as const) {
      const o = sendErrorOutcome(s, c);
      expect(o.waiting).toBe(false);
      expect(o.fallback).toBeUndefined();
      expect(o.error).toMatch(/paybill/);
    }
  });

  it("reloads when the bill changed", () => {
    expect(sendErrorOutcome(409, "not_payable")).toMatchObject({ refresh: true });
    expect(sendErrorOutcome(404, "not_found")).toMatchObject({ refresh: true });
  });
});
