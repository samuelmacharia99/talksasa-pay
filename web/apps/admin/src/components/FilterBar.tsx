import { buttonClass } from "@talksasa/ui";
import Link from "next/link";
import type { ReactNode } from "react";

/**
 * Filters are a plain GET form: they work without JavaScript and every
 * filtered view is a shareable URL.
 */
export function FilterBar({ action, children, active }: { action: string; children: ReactNode; active: boolean }) {
  return (
    <form method="get" action={action} className="mb-5 flex flex-wrap items-end gap-3">
      {children}
      <button type="submit" className={buttonClass("secondary", "md")}>
        Apply filters
      </button>
      {active ? (
        <Link href={action} className={buttonClass("ghost", "md")}>
          Clear
        </Link>
      ) : null}
    </form>
  );
}

export const filterInput =
  "h-10 rounded-md border border-line-strong bg-surface px-3 text-sm text-ink placeholder:text-faint focus:border-violet focus:outline-2 focus:outline-violet/25";

export function FilterField({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <label className={`grid gap-1 text-[0.8125rem] text-muted ${className ?? ""}`}>
      {label}
      {children}
    </label>
  );
}
