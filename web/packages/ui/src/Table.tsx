import type { ReactNode, TdHTMLAttributes, ThHTMLAttributes } from "react";

import { cx } from "./cx";

/** A dense, quiet data table. Horizontal scroll on small screens. */
export function Table({ children, label }: { children: ReactNode; label: string }) {
  return (
    <div className="overflow-x-auto">
      <table aria-label={label} className="w-full border-collapse text-sm">
        {children}
      </table>
    </div>
  );
}

export function Th({ className, align = "left", ...rest }: ThHTMLAttributes<HTMLTableCellElement> & { align?: "left" | "right" }) {
  return (
    <th
      scope="col"
      className={cx(
        "border-b border-line px-4 py-2.5 text-[0.75rem] font-medium whitespace-nowrap text-muted first:pl-5 last:pr-5",
        align === "right" ? "text-right" : "text-left",
        className,
      )}
      {...rest}
    />
  );
}

export function Td({ className, align = "left", ...rest }: TdHTMLAttributes<HTMLTableCellElement> & { align?: "left" | "right" }) {
  return (
    <td
      className={cx(
        "border-b border-line/70 px-4 py-3 align-middle first:pl-5 last:pr-5",
        align === "right" ? "text-right" : "text-left",
        className,
      )}
      {...rest}
    />
  );
}
