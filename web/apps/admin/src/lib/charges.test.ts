import { describe, expect, it } from "vitest";

import { chargeSubjects, chargeTimeline, checkoutURLFrom, txnRole } from "./charges";

const base = {
  status: "pending",
  created_at: "2026-11-25T08:00:00Z",
  succeeded_at: null,
  closed_at: null,
  transaction_id: null,
  stk_pushes: [],
  transactions: [],
};

describe("txnRole", () => {
  const t = { id: "t1", kind: "payment", direction: "in", status: "succeeded" };
  it("tells the paying transaction from duplicates and reversals", () => {
    expect(txnRole(t, "t1").key).toBe("paid");
    expect(txnRole(t, "t0").key).toBe("duplicate");
    expect(txnRole({ ...t, kind: "reversal", direction: "out" }, "t1").key).toBe("reversal");
    expect(txnRole({ ...t, status: "failed" }, null).label).toBe("Not paid");
    expect(txnRole({ ...t, status: "needs_review" }, null).key).toBe("review");
  });
});

describe("chargeTimeline", () => {
  it("is just Created for a fresh charge", () => {
    expect(chargeTimeline(base).map((e) => e.label)).toEqual(["Created"]);
  });

  it("orders prompts, payment, duplicate and reversal by time", () => {
    const tl = chargeTimeline({
      ...base,
      status: "reversed",
      succeeded_at: "2026-11-25T08:06:00Z",
      transaction_id: "t2",
      stk_pushes: [
        { id: "p2", status: "succeeded", created_at: "2026-11-25T08:05:00Z", completed_at: "2026-11-25T08:06:00Z", result_desc: null },
        { id: "p1", status: "failed", created_at: "2026-11-25T08:01:00Z", completed_at: "2026-11-25T08:02:00Z", result_desc: "Request cancelled by user" },
      ],
      transactions: [
        { id: "t1", kind: "payment", direction: "in", status: "failed", amount: 500, receipt: null, created_at: "2026-11-25T08:01:00Z", completed_at: "2026-11-25T08:02:00Z" },
        { id: "t2", kind: "payment", direction: "in", status: "succeeded", amount: 500, receipt: "SKL4ABCD12", created_at: "2026-11-25T08:05:00Z", completed_at: "2026-11-25T08:06:00Z" },
        { id: "t3", kind: "payment", direction: "in", status: "succeeded", amount: 500, receipt: "SKL4ABCD13", created_at: "2026-11-25T09:00:00Z", completed_at: "2026-11-25T09:00:00Z" },
        { id: "t4", kind: "reversal", direction: "out", status: "succeeded", amount: 500, receipt: null, created_at: "2026-11-25T10:00:00Z", completed_at: "2026-11-25T10:00:00Z" },
      ],
    });
    expect(tl.map((e) => e.label)).toEqual([
      "Created",
      "Payment prompt sent",
      "Prompt failed",
      "Payment prompt sent",
      "Paid",
      "Paid again (duplicate)",
      "Reversed",
    ]);
    expect(tl[2]?.detail).toBe("Request cancelled by user");
    expect(tl[4]?.href).toBe("/transactions/t2");
  });

  it("ends with Expired or Cancelled", () => {
    expect(chargeTimeline({ ...base, status: "expired", closed_at: "2026-11-26T08:00:00Z" }).at(-1)?.label).toBe("Expired");
    expect(chargeTimeline({ ...base, status: "cancelled", closed_at: "2026-11-26T08:00:00Z" }).at(-1)?.label).toBe("Cancelled by the app");
  });
});

describe("chargeSubjects", () => {
  it("collects the charge, its prompts and its transactions", () => {
    expect([...chargeSubjects({ id: "c", stk_pushes: [{ id: "p" }], transactions: [{ id: "t" }] })]).toEqual(["c", "p", "t"]);
  });
});

describe("checkoutURLFrom", () => {
  it("reads checkout_url from a charge event body", () => {
    expect(checkoutURLFrom({ checkout_url: "https://pay.example/abc" })).toBe("https://pay.example/abc");
    expect(checkoutURLFrom({ charge: { checkout_url: "https://pay.example/x" } })).toBe("https://pay.example/x");
    expect(checkoutURLFrom({ checkout_url: "javascript:alert(1)" })).toBeUndefined();
    expect(checkoutURLFrom(null)).toBeUndefined();
  });
});
