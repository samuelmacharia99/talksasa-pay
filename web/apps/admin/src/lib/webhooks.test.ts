import { describe, expect, it } from "vitest";

import {
  attemptTone,
  canRedeliver,
  checkAllowedIP,
  deliveryStatusLabel,
  prettyJSON,
  splitAllowedIPs,
  subjectHref,
  validateAllowedIPs,
} from "./webhooks";

describe("delivery status", () => {
  it("labels every status and falls back to readable text", () => {
    expect(deliveryStatusLabel("pending")).toBe("Pending");
    expect(deliveryStatusLabel("delivered")).toBe("Delivered");
    expect(deliveryStatusLabel("failed")).toBe("Failed");
    expect(deliveryStatusLabel("skipped")).toBe("Skipped");
    expect(deliveryStatusLabel("something_new")).toBe("something new");
  });
  it("allows redelivery only once a delivery has stopped without success", () => {
    expect(canRedeliver("failed")).toBe(true);
    expect(canRedeliver("skipped")).toBe(true);
    expect(canRedeliver("pending")).toBe(false);
    expect(canRedeliver("delivered")).toBe(false);
  });
  it("treats only 2xx attempts as successful", () => {
    expect(attemptTone(200)).toBe("ok");
    expect(attemptTone(204)).toBe("ok");
    expect(attemptTone(301)).toBe("bad");
    expect(attemptTone(500)).toBe("bad");
    expect(attemptTone(null)).toBe("bad");
  });
  it("links subjects to their panel pages", () => {
    expect(subjectHref("charge", "c1")).toBe("/charges/c1");
    expect(subjectHref("transaction", "t1")).toBe("/transactions/t1");
    expect(subjectHref("other", "x")).toBeUndefined();
  });
  it("links a payment prompt to its charge when the event names it", () => {
    const chargeId = "0193a7a4-1111-7000-8000-000000000001";
    expect(subjectHref("stk_push", "s1", { object: "stk_push", charge_id: chargeId })).toBe(`/charges/${chargeId}`);
    expect(subjectHref("stk_push", "s1")).toBeUndefined();
    expect(subjectHref("stk_push", "s1", { charge_id: "../../x" })).toBeUndefined();
  });
  it("pretty-prints JSON without throwing", () => {
    expect(prettyJSON({ a: 1 })).toBe('{\n  "a": 1\n}');
    const loop: Record<string, unknown> = {};
    loop.self = loop;
    expect(prettyJSON(loop)).toBe("[object Object]");
  });
});

describe("allowed IPs textarea", () => {
  it("means any address when empty", () => {
    expect(splitAllowedIPs("")).toBeNull();
    expect(splitAllowedIPs("  \n\n ")).toBeNull();
    expect(validateAllowedIPs("")).toEqual([]);
  });
  it("splits on lines, commas and spaces, dropping blanks and duplicates", () => {
    expect(splitAllowedIPs(" 203.0.113.4\n\n198.51.100.0/24, 203.0.113.4 2001:db8::/32\r\n")).toEqual([
      "203.0.113.4",
      "198.51.100.0/24",
      "2001:db8::/32",
    ]);
  });
  it("accepts addresses and ranges", () => {
    for (const ok of ["203.0.113.4", "10.0.0.0/8", "203.0.113.4/32", "2001:db8::1", "2001:db8::/32", "::1"]) {
      expect(checkAllowedIP(ok), ok).toBeUndefined();
    }
  });
  it("rejects things that are not addresses", () => {
    for (const bad of ["example.com", "300.1.1.1", "10.0.0", "10.0.0.0/0", "10.0.0.0/33", "2001:db8::/129", "1::2::3", "10.0.0.0/8/9", "10.0.0.0/x"]) {
      expect(checkAllowedIP(bad), bad).toBeDefined();
    }
  });
  it("reports every problem, including too many entries", () => {
    expect(validateAllowedIPs("203.0.113.4\nnope")).toEqual(["nope is not an IP address or CIDR range."]);
    const many = Array.from({ length: 51 }, (_, i) => `10.0.${i}.1`).join("\n");
    expect(validateAllowedIPs(many)).toEqual(["At most 50 entries. You have 51."]);
  });
});
