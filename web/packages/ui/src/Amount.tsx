import { formatAmount, formatKes } from "@talksasa/money";

import { cx } from "./cx";

/** A KES amount in tabular figures. `bare` drops the currency prefix. */
export function Amount({
  value,
  bare = false,
  signed = false,
  className,
}: {
  value: number;
  bare?: boolean;
  signed?: boolean;
  className?: string;
}) {
  const text = bare ? formatAmount(value) : formatKes(value);
  return (
    <span className={cx("num whitespace-nowrap", signed && value > 0 && "text-ok", signed && value < 0 && "text-bad", className)}>
      {signed && value > 0 ? "+" : ""}
      {text}
    </span>
  );
}
