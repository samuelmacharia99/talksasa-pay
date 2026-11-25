"use client";

import { useRouter } from "next/navigation";
import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { type StkSnapshot, stkActive } from "@/lib/stk";

const WATCH_FOR_MS = 30 * 60_000;
const SLOW_MS = 5000;
/** While a payment prompt is on the phone, check more often so the result shows quickly. */
const FAST_MS = 2500;

interface Live {
  stk: StkSnapshot | null;
  setStk: (s: StkSnapshot | null) => void;
  watching: boolean;
  /** Restart polling (e.g. after sending a prompt from a tab that had stopped). */
  wake: () => void;
}

const LiveContext = createContext<Live | null>(null);

export function useLive(): Live {
  const v = useContext(LiveContext);
  if (!v) throw new Error("useLive outside LiveProvider");
  return v;
}

/**
 * Polls the charge's status while the page is visible and refreshes the page
 * when the charge changes (e.g. the M-Pesa payment lands). Also tracks the
 * latest payment prompt for StkPanel. Stops after 30 minutes so a forgotten
 * tab does not poll forever.
 */
export function LiveProvider({
  token,
  status,
  initialStk,
  children,
}: {
  token: string;
  status: string;
  initialStk: StkSnapshot | null;
  children: ReactNode;
}) {
  const router = useRouter();
  const [stk, setStk] = useState<StkSnapshot | null>(initialStk);
  const [watching, setWatching] = useState(true);
  const [startedAt, setStartedAt] = useState(() => Date.now());
  const fast = stkActive(stk);

  useEffect(() => {
    if (!watching) return;
    let stopped = false;
    const tick = async () => {
      if (document.visibilityState !== "visible") return;
      if (Date.now() - startedAt > WATCH_FOR_MS) {
        setWatching(false);
        return;
      }
      try {
        const res = await fetch(`/api/status/${token}`, { cache: "no-store" });
        if (!res.ok || stopped) return;
        const s = (await res.json()) as { status: string; stk_push?: StkSnapshot | null };
        if (stopped) return;
        if (s.stk_push !== undefined) {
          setStk((prev) => (sameStk(prev, s.stk_push ?? null) ? prev : (s.stk_push ?? null)));
        }
        if (s.status !== status) router.refresh();
      } catch {
        // Offline for a moment; try again on the next tick.
      }
    };
    const id = setInterval(tick, fast ? FAST_MS : SLOW_MS);
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, [token, status, router, watching, startedAt, fast]);

  const wake = useCallback(() => {
    setStartedAt(Date.now());
    setWatching(true);
  }, []);

  const value = useMemo(() => ({ stk, setStk, watching, wake }), [stk, watching, wake]);
  return <LiveContext.Provider value={value}>{children}</LiveContext.Provider>;
}

function sameStk(a: StkSnapshot | null, b: StkSnapshot | null): boolean {
  return a === b || (!!a && !!b && a.status === b.status && a.message === b.message);
}

/** The small "this page updates by itself" line. */
export function LiveStatus() {
  const { watching } = useLive();
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
