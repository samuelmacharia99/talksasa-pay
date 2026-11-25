import { describe, expect, it } from "vitest";

import { checkSettingsForm, credentialLabel, maskCallbackURL, needsConfirmation } from "./settings";

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
  const ok = { shortcode: "174379", stk_shortcode: "", callback_base_url: "https://api.example/", https_only: true };
  it("accepts good settings", () => {
    expect(checkSettingsForm(ok)).toBeUndefined();
    expect(checkSettingsForm({ ...ok, callback_base_url: "http://api:8088/", https_only: false })).toBeUndefined();
  });
  it("points at the field to fix", () => {
    expect(checkSettingsForm({ ...ok, shortcode: "12ab" })?.field).toBe("shortcode");
    expect(checkSettingsForm({ ...ok, stk_shortcode: "1" })?.field).toBe("stk_shortcode");
    expect(checkSettingsForm({ ...ok, callback_base_url: "http://api.example/" })?.field).toBe("callback_base_url");
    expect(checkSettingsForm({ ...ok, callback_base_url: "https://api.example" })?.message).toContain("end with /");
    expect(checkSettingsForm({ ...ok, callback_base_url: "not a url" })?.field).toBe("callback_base_url");
  });
});
