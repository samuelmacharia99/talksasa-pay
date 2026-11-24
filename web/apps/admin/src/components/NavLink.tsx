"use client";

import { cx } from "@talksasa/ui";
import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

export function NavLink({ href, children }: { href: string; children: ReactNode }) {
  const path = usePathname();
  const active = href === "/" ? path === "/" : path === href || path.startsWith(href + "/");
  return (
    <Link
      href={href}
      aria-current={active ? "page" : undefined}
      className={cx(
        "relative flex h-9 items-center rounded-md px-3 text-sm whitespace-nowrap transition-colors",
        active ? "bg-white/10 font-medium text-white" : "text-white/65 hover:bg-white/5 hover:text-white",
      )}
    >
      {active ? <span aria-hidden className="tsp-tail absolute inset-y-2 left-0 hidden w-0.5 rounded-full lg:block" /> : null}
      {children}
    </Link>
  );
}

/**
 * A labelled section of the sidebar. On small screens the sidebar is one
 * scrolling row, so the label is hidden and the links sit inline.
 */
export function NavGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div role="group" aria-label={label} className="contents lg:mt-5 lg:block lg:space-y-0.5">
      <p aria-hidden className="mb-1.5 hidden px-3 text-[0.6875rem] font-medium tracking-[0.08em] text-white/40 uppercase lg:block">
        {label}
      </p>
      {children}
    </div>
  );
}
