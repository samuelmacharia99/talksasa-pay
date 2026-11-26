import { describe, expect, it } from "vitest";

import { checkTestPayment, testOutcome } from "./testpay";

const push = (status: string, desc: string | null = null, code: string | null = null) => ({ status, result_desc: desc, result_code: code });

describe("testOutcome", () => {
  it("is paid only when the charge succeeded", () => {
    expect(testOutcome({ charge_status: "succeeded", prompt_error: null, push: push("succeeded") })).toMatchObject({ label: "Paid", final: true });
    // A succeeded prompt whose charge is not updated yet is still in progress.
    expect(testOutcome({ charge_status: "pending", prompt_error: null, push: push("succeeded") })).toMatchObject({ final: false });
  });
  it("keeps polling while the prompt is live", () => {
    expect(testOutcome({ charge_status: "pending", prompt_error: null, push: push("initiating") }).final).toBe(false);
    expect(testOutcome({ charge_status: "pending", prompt_error: null, push: push("sent") }).final).toBe(false);
  });
  it("explains a failure with Safaricom's words", () => {
    const o = testOutcome({ charge_status: "pending", prompt_error: null, push: push("failed", "Request cancelled by user", "1032") });
    expect(o).toMatchObject({ label: "Not paid", tone: "bad", final: true, detail: "Request cancelled by user (code 1032)" });
  });
  it("shows why no prompt was sent", () => {
    expect(testOutcome({ charge_status: "pending", prompt_error: "Safaricom refused", push: null })).toMatchObject({
      label: "Prompt not sent",
      detail: "Safaricom refused",
    });
  });
  it("never reads needs_review or no answer as paid", () => {
    expect(testOutcome({ charge_status: "needs_review", prompt_error: null, push: push("unknown") }).label).toBe("Needs review");
    expect(testOutcome({ charge_status: "pending", prompt_error: null, push: push("unknown") }).tone).toBe("warn");
  });
});

describe("checkTestPayment", () => {
  it("accepts KES 1 to 1,000 and Safaricom numbers", () => {
    expect(checkTestPayment({ amount: "1", phone: "0712 345 678" })).toBeUndefined();
    expect(checkTestPayment({ amount: "1000", phone: "+254 112 345 678" })).toBeUndefined();
  });
  it("rejects other amounts", () => {
    for (const amount of ["0", "1001", "10.5", "", "-5"]) expect(checkTestPayment({ amount, phone: "0712345678" })?.field).toBe("amount");
  });
  it("rejects other numbers", () => {
    for (const phone of ["0812345678", "071234", "", "254712"]) expect(checkTestPayment({ amount: "10", phone })?.field).toBe("phone");
  });
});
