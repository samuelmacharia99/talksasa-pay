import { describe, expect, it } from "vitest";

import { attentionItems, type DashboardCounts, nairobiToday, overviewTiles } from "./overview";

const zero: DashboardCounts = {
  charges_pending: 0,
  charges_succeeded_today: 0,
  charges_succeeded_today_amount: 0,
  charges_needs_review: 0,
  transactions_needs_review: 0,
  unmatched_count: 0,
  unmatched_amount: 0,
  pending_transactions: 0,
  unknown_stk_pushes: 0,
  unprocessed_callbacks: 0,
  inactive_app_payments: 0,
};

describe("overviewTiles", () => {
  it("links each count to its filtered list", () => {
    const tiles = overviewTiles({ ...zero, charges_pending: 4, charges_succeeded_today: 1, charges_succeeded_today_amount: 1500 }, "2026-11-25");
    expect(tiles.map((t) => t.href)).toEqual([
      "/charges?status=pending",
      "/charges?status=succeeded&from=2026-11-25",
      "/transactions?status=needs_review",
      "/unmatched",
      "/mpesa/callbacks?unprocessed=true",
      "/apps",
    ]);
    expect(tiles[0]?.value).toBe("4");
    expect(tiles[1]?.detail).toBe("1 charge paid since midnight");
  });

  it("colours only counts that need someone", () => {
    expect(overviewTiles(zero, "2026-11-25").slice(2).every((t) => t.tone === "neutral")).toBe(true);
    expect(overviewTiles({ ...zero, transactions_needs_review: 2 }, "2026-11-25")[2]?.tone).toBe("bad");
  });
});

describe("attentionItems", () => {
  it("is empty when nothing needs attention", () => {
    expect(attentionItems(zero, null)).toEqual([]);
    expect(attentionItems(zero, { count: 0, more: false })).toEqual([]);
  });

  it("puts urgent items first", () => {
    const items = attentionItems({ ...zero, unmatched_count: 2, unmatched_amount: 900, transactions_needs_review: 1, unprocessed_callbacks: 3 }, { count: 100, more: true });
    expect(items.map((i) => i.urgent)).toEqual([true, true, true, false]);
    expect(items[0]?.label).toBe("1 payment needs review");
    expect(items[2]?.label).toBe("100+ webhooks failed");
    expect(items[3]?.label).toBe("2 unmatched payments");
  });
});

describe("nairobiToday", () => {
  it("uses Kenyan time", () => {
    expect(nairobiToday(new Date("2026-11-24T22:30:00Z"))).toBe("2026-11-25");
  });
});
