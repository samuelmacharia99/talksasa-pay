"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";

/**
 * Polls for payment every 5 seconds while the page is visible and refreshes
 * the page when the bill changes (e.g. the M-Pesa payment lands). Stops
 * after 30 minutes so a forgotten tab does not poll forever.
 */
export function LiveStatus({ token, status, amountPaid }: { token: string; status: string; amountPaid: number }) {
  const router = useRouter();
  const [watching, setWatching] = useState(true);

  useEffect(() => {
    if (!watching) return;
    let stopped = false;
    const startedAt = Date.now();
    const tick = async () => {
      if (document.visibilityState !== "visible") return;
      if (Date.now() - startedAt > 30 * 60_000) {
        setWatching(false);
        return;
      }
      try {
        const res = await fetch(`/api/status/${token}`, { cache: "no-store" });
        if (!res.ok || stopped) return;
        const s = (await res.json()) as { status: string; amount_paid: number };
        if (s.status !== status || s.amount_paid !== amountPaid) router.refresh();
      } catch {
        // Offline for a moment; try again on the next tick.
      }
    };
    const id = setInterval(tick, 5000);
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, [token, status, amountPaid, router, watching]);

  return (
    <p className="mt-5 flex items-center gap-2 text-[0.8125rem] text-muted" aria-live="polite">
      {watching ? (
        <>
          <span aria-hidden className="relative flex size-2">
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-violet/50 motion-reduce:hidden" />
            <span className="relative inline-flex size-2 rounded-full bg-violet" />
          </span>
          This page updates by itself when your payment arrives.
        </>
      ) : (
        <>Reload the page to check for your payment.</>
      )}
    </p>
  );
}
