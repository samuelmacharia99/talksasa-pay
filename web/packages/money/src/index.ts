/**
 * Display helpers for Kenya shilling amounts.
 *
 * The frontends never compute money: every amount, balance and status comes
 * from the Go service as an integer number of whole shillings. These helpers
 * only validate and format what the server sent.
 */

declare const kesBrand: unique symbol;

/** A whole number of Kenya shillings, as returned by the API. */
export type Kes = number & { readonly [kesBrand]: true };

/** Largest amount the service accepts (KES 1 trillion). */
export const MAX_KES = 1_000_000_000_000;

/** Narrows an API number to Kes, rejecting fractions and unsafe integers. */
export function toKes(n: number): Kes {
  if (!Number.isSafeInteger(n) || Math.abs(n) > MAX_KES) {
    throw new RangeError(`not a whole-shilling amount: ${n}`);
  }
  return n as Kes;
}

const grouped = new Intl.NumberFormat("en-KE", { maximumFractionDigits: 0, useGrouping: true });

/** "KES 1,234" (negative: "−KES 1,234" with a true minus sign). */
export function formatKes(n: number): string {
  const k = toKes(n);
  const body = `KES ${grouped.format(Math.abs(k))}`;
  return k < 0 ? `−${body}` : body;
}

/** "1,234" without the currency, for tables that label the column. */
export function formatAmount(n: number): string {
  const k = toKes(n);
  return (k < 0 ? "−" : "") + grouped.format(Math.abs(k));
}

/** Compact form for chart axes: 950, 12.5K, 3.2M. */
export function formatKesCompact(n: number): string {
  const k = toKes(n);
  const abs = Math.abs(k);
  const sign = k < 0 ? "−" : "";
  if (abs < 1_000) return `${sign}${abs}`;
  if (abs < 1_000_000) return `${sign}${trim(abs / 1_000)}K`;
  if (abs < 1_000_000_000) return `${sign}${trim(abs / 1_000_000)}M`;
  return `${sign}${trim(abs / 1_000_000_000)}B`;
}

function trim(x: number): string {
  return (Math.round(x * 10) / 10).toString();
}
