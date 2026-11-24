import { describe, expect, it } from "vitest";

import { callbackState, mpesaAttention, normalizeReceipt, stkResult, stkStatus, validateResolve } from "./mpesa";

describe("stkStatus", () => {
  it("labels every prompt status in plain words", () => {
    expect(stkStatus("initiating").label).toBe("Sending");
    expect(stkStatus("sent").label).toBe("On the phone");
    expect(stkStatus("succeeded")).toEqual({ label: "Paid", tone: "ok" });
    expect(stkStatus("failed").tone).toBe("bad");
    expect(stkStatus("unknown")).toEqual({ label: "No answer", tone: "warn" });
    expect(stkStatus("brand_new")).toEqual({ label: "brand new", tone: "neutral" });
  });

  it("shows Safaricom's result with its code", () => {
    expect(stkResult({ result_code: "1032", result_desc: "Request cancelled by user" })).toBe("Request cancelled by user (code 1032)");
    expect(stkResult({ result_code: "1", result_desc: null })).toBe("Code 1");
    expect(stkResult({ result_code: null, result_desc: null })).toBeNull();
  });
});

describe("callbackState", () => {
  it("is processed, failing or waiting", () => {
    expect(callbackState({ processed_at: "2026-11-24T10:00:00Z", error: "old" }).label).toBe("Processed");
    expect(callbackState({ processed_at: null, error: "boom" })).toEqual({ label: "Failing", tone: "bad" });
    expect(callbackState({ processed_at: null, error: null }).label).toBe("Waiting");
  });
});

describe("validateResolve", () => {
  it("requires a well-formed receipt to confirm", () => {
    expect(validateResolve({ action: "confirm", receipt: "", reason: "Found on the portal" }).receipt).toBeDefined();
    expect(validateResolve({ action: "confirm", receipt: "SKL-4ABCD12", reason: "Found on the portal" }).receipt).toBeDefined();
    expect(validateResolve({ action: "confirm", receipt: " skl4abcd12 ", reason: "Found on the portal" })).toEqual({});
  });

  it("does not need a receipt to mark not paid", () => {
    expect(validateResolve({ action: "fail", receipt: "", reason: "Not on the portal" })).toEqual({});
  });

  it("always needs a reason of 3-500 characters", () => {
    expect(validateResolve({ action: "fail", receipt: "", reason: " x " }).reason).toBeDefined();
    expect(validateResolve({ action: "fail", receipt: "", reason: "y".repeat(501) }).reason).toBeDefined();
  });

  it("rejects unknown actions", () => {
    expect(validateResolve({ action: "approve" as "fail", receipt: "", reason: "abc" }).action).toBeDefined();
  });

  it("normalises receipts", () => {
    expect(normalizeReceipt(" skl4 abcd12\n")).toBe("SKL4ABCD12");
  });
});

describe("mpesaAttention", () => {
  it("is empty when the counts are absent or zero", () => {
    expect(mpesaAttention({})).toEqual([]);
    expect(mpesaAttention({ unknown_stk_pushes: 0, unprocessed_callbacks: 0, inactive_product_payments: 0 })).toEqual([]);
  });

  it("lists each non-zero count with a link", () => {
    const items = mpesaAttention({ unknown_stk_pushes: 1, unprocessed_callbacks: 3, inactive_product_payments: 2 });
    expect(items.map((i) => i.href)).toEqual(["/transactions?status=needs_review&channel=mpesa_stk", "/mpesa/callbacks?unprocessed=true", "/transactions"]);
    expect(items[0]?.label).toBe("1 payment prompt has no answer");
    expect(items[1]?.label).toBe("3 M-Pesa callbacks not processed");
    expect(items[0]?.urgent).toBe(true);
    expect(items[2]?.urgent).toBe(false);
  });
});
