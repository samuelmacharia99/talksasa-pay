"use client";

import { cx } from "@talksasa/ui";
import Link from "next/link";
import { usePathname } from "next/navigation";

export type DocsNavItem = { href: string; label: string };

/** The guide list beside the Developers pages (a scrolling row on small screens). */
export function DocsNav({ guides, reference }: { guides: DocsNavItem[]; reference: DocsNavItem[] }) {
  const path = usePathname();
  const item = (i: DocsNavItem) => {
    const active = path === i.href;
    return (
      <li key={i.href}>
        <Link
          href={i.href}
          aria-current={active ? "page" : undefined}
          className={cx(
            "block rounded-md px-3 py-1.5 text-sm whitespace-nowrap transition-colors lg:whitespace-normal",
            active ? "bg-violet-soft font-medium text-violet-deep" : "text-ink-2 hover:bg-wash hover:text-ink",
          )}
        >
          {i.label}
        </Link>
      </li>
    );
  };
  return (
    <nav aria-label="Developer guides" className="lg:sticky lg:top-8">
      <p className="mb-1.5 hidden px-3 text-[0.75rem] font-medium tracking-wide text-faint uppercase lg:block">Guides</p>
      <ul className="-mx-1 flex gap-1 overflow-x-auto pb-2 lg:mx-0 lg:block lg:space-y-0.5 lg:overflow-visible lg:pb-0">
        {guides.map(item)}
        <li aria-hidden className="mx-1 w-px shrink-0 bg-line lg:hidden" />
        <li className="mt-5 mb-1.5 hidden px-3 text-[0.75rem] font-medium tracking-wide text-faint uppercase lg:block">Reference</li>
        {reference.map(item)}
      </ul>
    </nav>
  );
}
