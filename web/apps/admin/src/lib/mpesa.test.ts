import { describe, expect, it } from "vitest";

import {
  callbackState,
  normalizeReceipt,
  reverseConfirmWord,
  stkResult,
  stkStatus,
  validateAssign,
  validateResolve,
  validateReverse,
} from "./mpesa";

describe("validateReverse", () => {
  const ok = { reason: "Bank recall, ticket 4411", receipt: "SRV1ABCD23", receiptRequired: true, confirm: "skl4abcd12", expected: "SKL4ABCD12" };

  it("accepts a complete form, ignoring case in the confirmation", () => {
    expect(validateReverse(ok)).toEqual({});
  });

  it("needs the reversal receipt for M-Pesa payments only", () => {
    expect(validateReverse({ ...ok, receipt: "" }).receipt).toBeDefined();
    expect(validateReverse({ ...ok, receipt: "", receiptRequired: false })).toEqual({});
    expect(validateReverse({ ...ok, receipt: "SKL-4ABCD" }).receipt).toBeDefined();
  });

  it("needs a reason and the exact confirmation", () => {
    expect(validateReverse({ ...ok, reason: "x" }).reason).toBeDefined();
    expect(validateReverse({ ...ok, confirm: "SKL4ABCD1" }).confirm).toBe("Type SKL4ABCD12 to confirm.");
  });

  it("asks for REVERSE when the payment has no receipt", () => {
    expect(reverseConfirmWord(null)).toBe("REVERSE");
    expect(reverseConfirmWord("skl4abcd12")).toBe("SKL4ABCD12");
  });
});

describe("validateAssign", () => {
  it("needs an app and a reason", () => {
    expect(validateAssign({ appId: "", reason: "" })).toEqual({ app_id: expect.any(String), reason: expect.any(String) });
    expect(validateAssign({ appId: "0193a7a4-1111-7000-8000-000000000001", reason: "Typo in prefix, confirmed" })).toEqual({});
  });
});

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

