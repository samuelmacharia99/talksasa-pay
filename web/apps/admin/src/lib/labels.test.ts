import { describe, expect, it } from "vitest";

import { actorLabel, entityHref } from "./labels";
import { anySet, pick } from "./params";

describe("actorLabel", () => {
  it("names staff from the directory and never shows raw ids", () => {
    const names = new Map([["0193-abc", "Wanjiku Kamau"]]);
    expect(actorLabel("admin:0193-abc", names)).toBe("Wanjiku Kamau");
    expect(actorLabel("admin:unknown-id", names)).toBe("Staff member");
  });
  it("describes non-staff actors", () => {
    expect(actorLabel("cli:Jane")).toBe("Jane (command line)");
    expect(actorLabel("app:0193")).toBe("App API");
    expect(actorLabel("system:worker")).toBe("System (worker)");
  });
});

describe("entityHref", () => {
  it("links apps, charges and transactions to their pages", () => {
    expect(entityHref("app", "a1")).toBe("/apps/a1");
    expect(entityHref("charge", "c1")).toBe("/charges/c1");
    expect(entityHref("transaction", "t1")).toBe("/transactions/t1");
    expect(entityHref("admin_user", "u1")).toBeUndefined();
    expect(entityHref("charge", null)).toBeUndefined();
  });
});

describe("search params", () => {
  it("keeps only non-empty single string values", () => {
    const p = pick({ status: "succeeded", q: "  ", app_id: ["a", "b"], extra: "x" }, ["status", "q", "app_id"] as const);
    expect(p).toEqual({ status: "succeeded", q: undefined, app_id: undefined });
  });
  it("treats the cursor alone as no filter", () => {
    expect(anySet({ cursor: "abc", status: undefined })).toBe(false);
    expect(anySet({ cursor: undefined, status: "pending" })).toBe(true);
  });
});
