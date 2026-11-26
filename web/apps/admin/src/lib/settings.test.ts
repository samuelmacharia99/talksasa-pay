import { describe, expect, it } from "vitest";

import { callbackTokenLabel, checkSettingsForm, credentialLabel, derivedCallbackBase, maskCallbackURL, needsConfirmation } from "./settings";

describe("credentialLabel", () => {
  it("never shows more than the last four characters", () => {
    expect(credentialLabel({ set: true, last4: "1234" })).toBe("Saved ••••1234");
    expect(credentialLabel({ set: true, last4: null })).toBe("Saved");
    expect(credentialLabel({ set: false, last4: null })).toBe("Not set");
  });
});

describe("maskCallbackURL", () => {
  it("hides the callback token", () => {
    expect(maskCallbackURL("https://api.example/cb/mpesa/[TOKEN]/c2b/confirmation")).toBe("https://api.example/cb/mpesa/••••••••/c2b/confirmation");
  });
});

describe("needsConfirmation", () => {
  it("asks whenever production is involved", () => {
    expect(needsConfirmation(null, "sandbox")).toBe(false);
    expect(needsConfirmation("sandbox", "production")).toBe(true);
    expect(needsConfirmation("production", "sandbox")).toBe(true);
  });
});

describe("checkSettingsForm", () => {
  const ok = { shortcode: "174379", stk_shortcode: "" };
  it("accepts good settings", () => {
    expect(checkSettingsForm(ok)).toBeUndefined();
    expect(checkSettingsForm({ ...ok, stk_shortcode: "600111" })).toBeUndefined();
  });
  it("points at the field to fix", () => {
    expect(checkSettingsForm({ ...ok, shortcode: "12ab" })?.field).toBe("shortcode");
    expect(checkSettingsForm({ ...ok, stk_shortcode: "1" })?.field).toBe("stk_shortcode");
  });
});

describe("derivedCallbackBase", () => {
  it("is this panel's public address plus /api/", () => {
    expect(derivedCallbackBase("user-4-service-641-go.talksasa.live", "https")).toBe("https://user-4-service-641-go.talksasa.live/api/");
    expect(derivedCallbackBase("Pay.Example.com", null)).toBe("https://pay.example.com/api/");
    expect(derivedCallbackBase("pay.example.com, edge", "https, http")).toBe("https://pay.example.com/api/");
    expect(derivedCallbackBase("127.0.0.1:3001", null)).toBe("http://127.0.0.1:3001/api/");
  });
  it("refuses anything that is not a plain host", () => {
    for (const h of [null, "", "evil.com/x", "a b", "user@host", ".example.com", "host:99999999"]) {
      expect(derivedCallbackBase(h, "https")).toBeNull();
    }
  });
});

describe("callbackTokenLabel", () => {
  it("says where the token comes from", () => {
    expect(callbackTokenLabel("deployment")).toContain("TSP_MPESA_CALLBACK_TOKEN");
    expect(callbackTokenLabel("generated")).toContain("Generated");
    expect(callbackTokenLabel(null)).toContain("when you save");
  });
});
