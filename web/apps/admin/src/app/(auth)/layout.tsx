import type { ReactNode } from "react";

import { Wordmark } from "@talksasa/ui";

/**
 * Sign-in and setup screens: a narrow ink panel carrying the brand "tail"
 * on the left, the form on paper on the right. Single column on phones.
 */
export default function AuthLayout({ children }: { children: ReactNode }) {
  return (
    <div className="grid min-h-dvh lg:grid-cols-[minmax(320px,420px)_1fr]">
      <aside className="relative hidden overflow-hidden bg-ink px-10 py-12 text-white lg:flex lg:flex-col">
        <span aria-hidden className="tsp-tail absolute inset-y-0 right-0 w-1.5" />
        <Wordmark onDark />
        <div className="mt-auto max-w-[17rem]">
          <p className="font-display text-[1.65rem] leading-[1.2] font-semibold tracking-[-0.015em]">
            One paybill. Every Talksasa product.
          </p>
          <p className="mt-4 text-sm leading-relaxed text-white/65">
            Staff console for collections, reconciliation and refunds. Every action you take here is recorded under your name.
          </p>
        </div>
      </aside>
      <main className="flex flex-col px-6 py-10 sm:px-10">
        <div className="lg:hidden">
          <Wordmark />
        </div>
        <div className="my-auto w-full max-w-sm py-10 lg:mx-auto">{children}</div>
      </main>
    </div>
  );
}
