import Link from "next/link";

import { buttonClass } from "@talksasa/ui";

export default function NotFound() {
  return (
    <main className="mx-auto max-w-lg px-6 py-24">
      <h1 className="font-display text-2xl font-semibold">Nothing here</h1>
      <p className="mt-3 text-ink-2">The record may not exist, or the link is wrong.</p>
      <Link href="/" className={buttonClass("secondary", "md", "mt-6")}>
        Back to overview
      </Link>
    </main>
  );
}
