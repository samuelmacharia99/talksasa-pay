import Link from "next/link";

import { buttonClass } from "@talksasa/ui";

export default function Forbidden() {
  return (
    <main className="mx-auto max-w-lg px-6 py-24">
      <h1 className="font-display text-2xl font-semibold">Your role can’t open this page</h1>
      <p className="mt-3 text-ink-2">Ask an admin to change your role if you need access.</p>
      <Link href="/" className={buttonClass("secondary", "md", "mt-6")}>
        Back to overview
      </Link>
    </main>
  );
}
