"use client";

import { Button } from "@talksasa/ui";

export default function ErrorPage({ reset }: { error: Error; reset: () => void }) {
  return (
    <main className="mx-auto max-w-[30rem] px-4 py-16">
      <h1 className="font-display text-2xl font-semibold">We couldn’t load this bill</h1>
      <p className="mt-3 text-ink-2">
        Our payment service didn’t respond. Your link is fine; try again in a moment. If you already paid, your money is safe.
      </p>
      <Button className="mt-6" onClick={reset}>
        Try again
      </Button>
    </main>
  );
}
