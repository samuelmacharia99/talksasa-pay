import { describe, expect, it } from "vitest";
import { formatAmount, formatKes, formatKesCompact, toKes } from "./index";

describe("money", () => {
  it("formats whole shillings with grouping", () => {
    expect(formatKes(0)).toBe("KES 0");
    expect(formatKes(1234)).toBe("KES 1,234");
    expect(formatKes(1_000_000)).toBe("KES 1,000,000");
    expect(formatKes(-500)).toBe("−KES 500");
    expect(formatAmount(45000)).toBe("45,000");
  });

  it("rejects anything that is not a whole-shilling amount", () => {
    for (const bad of [1.5, Number.NaN, Infinity, 2 ** 60, 1e13]) {
      expect(() => toKes(bad)).toThrow(RangeError);
      expect(() => formatKes(bad)).toThrow(RangeError);
    }
  });

  it("compacts for chart axes", () => {
    expect(formatKesCompact(950)).toBe("950");
    expect(formatKesCompact(12_500)).toBe("12.5K");
    expect(formatKesCompact(3_200_000)).toBe("3.2M");
  });
});
