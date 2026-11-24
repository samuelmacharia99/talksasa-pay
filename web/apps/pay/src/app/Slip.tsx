import { Wordmark } from "@talksasa/ui";
import type { ReactNode } from "react";

/**
 * The bill as a paper slip: the brand tail across the top, a perforation
 * between "what you owe" and "how to pay". The one bold element on the page.
 */
export function Slip({ top, bottom }: { top: ReactNode; bottom?: ReactNode }) {
  return (
    <main className="mx-auto flex min-h-dvh max-w-[30rem] flex-col px-4 py-8 sm:py-14">
      <div className="mb-6 px-1">
        <Wordmark />
      </div>
      <article className="overflow-hidden rounded-lg border border-line bg-surface shadow-[0_1px_0_#e3e0ee,0_18px_40px_-24px_rgba(27,21,48,0.35)]">
        <div aria-hidden className="tsp-tail-x h-1.5" />
        <div className="px-6 pt-6 pb-7 sm:px-8">{top}</div>
        {bottom ? (
          <>
            <div aria-hidden className="relative h-5">
              <span className="absolute top-1/2 -left-2.5 size-5 -translate-y-1/2 rounded-full border border-line bg-paper" />
              <span className="absolute top-1/2 -right-2.5 size-5 -translate-y-1/2 rounded-full border border-line bg-paper" />
              <span className="absolute inset-x-5 top-1/2 border-t-2 border-dashed border-line" />
            </div>
            <div className="px-6 pt-5 pb-7 sm:px-8">{bottom}</div>
          </>
        ) : null}
      </article>
      <p className="mt-6 px-1 text-[0.8125rem] leading-relaxed text-muted">
        Talksasa Home of Creatives Ltd. Keep your M-Pesa confirmation SMS until this page shows the bill as paid.
      </p>
    </main>
  );
}
