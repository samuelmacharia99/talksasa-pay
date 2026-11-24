import { describe, expect, it } from "vitest";

import { actorLabel } from "./labels";
import { anySet, pick } from "./params";

describe("actorLabel", () => {
  it("names staff from the directory and never shows raw ids", () => {
    const names = new Map([["0193-abc", "Wanjiku Kamau"]]);
    expect(actorLabel("admin:0193-abc", names)).toBe("Wanjiku Kamau");
    expect(actorLabel("admin:unknown-id", names)).toBe("Staff member");
  });
  it("describes non-staff actors", () => {
    expect(actorLabel("cli:Jane")).toBe("Jane (command line)");
    expect(actorLabel("product:0193")).toBe("Product API");
    expect(actorLabel("system:worker")).toBe("System (worker)");
  });
});

describe("search params", () => {
  it("keeps only non-empty single string values", () => {
    const p = pick({ status: "paid", q: "  ", product_id: ["a", "b"], extra: "x" }, ["status", "q", "product_id"] as const);
    expect(p).toEqual({ status: "paid", q: undefined, product_id: undefined });
  });
  it("treats the cursor alone as no filter", () => {
    expect(anySet({ cursor: "abc", status: undefined })).toBe(false);
    expect(anySet({ cursor: undefined, status: "open" })).toBe(true);
  });
});
