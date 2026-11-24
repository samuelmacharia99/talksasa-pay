"use client";

import { Button, cx, inputClass } from "@talksasa/ui";
import { useRouter } from "next/navigation";
import { type FormEvent, useId, useRef, useState } from "react";

import { normalizeKenyanPhone, PHONE_ERROR, PHONE_HINT, sendErrorOutcome, type StkSnapshot, stkView } from "@/lib/stk";

import { useLive } from "./LiveStatus";

/**
 * "Send payment prompt": asks for the customer's Safaricom number and has
 * M-Pesa show the payment on their phone. The request goes to our own
 * /api/stk route (never to the Go service directly). The result arrives
 * through LiveProvider's polling.
 */
export function StkPanel({ token, amountLabel, canSend }: { token: string; amountLabel: string; canSend: boolean }) {
  const router = useRouter();
  const { stk, setStk, wake } = useLive();
  const [phone, setPhone] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldError, setFieldError] = useState(false);
  const [fallback, setFallback] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const id = useId();
  const inputId = `${id}-phone`;
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;

  const view = stkView(stk);
  const showForm = canSend && !fallback && (view.kind === "form" || (view.kind === "failed" && retrying));

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (sending) return;
    const normalized = normalizeKenyanPhone(phone);
    if (!normalized) {
      setFieldError(true);
      setError(PHONE_ERROR);
      inputRef.current?.focus();
      return;
    }
    setSending(true);
    setError(null);
    setFieldError(false);
    try {
      const res = await fetch(`/api/stk/${token}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ phone: normalized }),
        cache: "no-store",
      });
      const body = (await res.json().catch(() => ({}))) as { code?: string; stk_push?: StkSnapshot };
      if (res.status === 202 && body.stk_push) {
        setRetrying(false);
        setStk(body.stk_push);
        wake();
        return;
      }
      const o = sendErrorOutcome(res.status, body.code);
      if (o.waiting) {
        setRetrying(false);
        setStk({ status: "sent", message: null });
        wake();
        return;
      }
      setError(o.error ?? null);
      setFieldError(!!o.fieldError);
      if (o.fallback) setFallback(true);
      if (o.refresh) router.refresh();
      if (o.fieldError) inputRef.current?.focus();
    } catch {
      setError(sendErrorOutcome(0, undefined).error ?? null);
    } finally {
      setSending(false);
    }
  }

  return (
    <section aria-labelledby={`${id}-title`} className="mb-6 border-b border-dashed border-line pb-6">
      <h2 id={`${id}-title`} className="font-display text-base font-semibold">
        Pay from your phone
      </h2>

      <div role="status" aria-live="polite">
        {view.kind !== "form" && !(view.kind === "failed" && retrying) ? <StkState view={view} /> : null}
      </div>

      {view.kind === "failed" && !retrying && canSend && !fallback ? (
        <Button
          className="mt-4 w-full sm:w-auto"
          onClick={() => {
            setRetrying(true);
            setError(null);
            requestAnimationFrame(() => inputRef.current?.focus());
          }}
        >
          Try again
        </Button>
      ) : null}

      {showForm ? (
        <form onSubmit={submit} noValidate className="mt-3 grid gap-3">
          <p className="text-[0.9375rem] text-ink-2">
            We’ll send an M-Pesa prompt for <span className="font-medium text-ink">{amountLabel}</span> to your phone. Enter your PIN
            on the phone to pay.
          </p>
          <div className="grid gap-1.5">
            <label htmlFor={inputId} className="text-[0.8125rem] font-medium text-ink-2">
              M-Pesa phone number
            </label>
            <input
              ref={inputRef}
              id={inputId}
              name="phone"
              type="tel"
              inputMode="tel"
              autoComplete="tel"
              placeholder="0712 345 678"
              maxLength={20}
              required
              value={phone}
              onChange={(e) => {
                setPhone(e.target.value);
                if (fieldError) {
                  setFieldError(false);
                  setError(null);
                }
              }}
              aria-invalid={fieldError ? true : undefined}
              aria-describedby={fieldError ? errorId : hintId}
              className={cx(inputClass, "h-12 text-base")}
            />
            {!fieldError ? (
              <p id={hintId} className="text-[0.8125rem] text-muted">
                {PHONE_HINT}
              </p>
            ) : null}
          </div>
          {error ? (
            <p id={errorId} role="alert" className={cx("text-[0.875rem]", fieldError ? "text-bad" : "rounded-md bg-warn-soft px-3 py-2 text-warn")}>
              {error}
            </p>
          ) : null}
          <Button type="submit" disabled={sending} aria-disabled={sending} className="h-12 w-full text-[0.9375rem]">
            {sending ? "Sending…" : "Send payment prompt"}
          </Button>
        </form>
      ) : error ? (
        <p role="alert" className="mt-3 rounded-md bg-warn-soft px-3 py-2 text-[0.875rem] text-warn">
          {error}
        </p>
      ) : null}
    </section>
  );
}

function StkState({ view }: { view: Exclude<ReturnType<typeof stkView>, { kind: "form" }> }) {
  const tone = {
    waiting: "border-violet/30 bg-violet-soft",
    confirming: "border-info/30 bg-info-soft",
    received: "border-ok/30 bg-ok-soft",
    failed: "border-bad/30 bg-bad-soft",
  }[view.kind];
  return (
    <div className={cx("mt-3 flex gap-3 rounded-md border px-4 py-3", tone)}>
      {view.kind === "waiting" || view.kind === "confirming" ? (
        <span aria-hidden className="relative mt-1.5 flex size-2 shrink-0">
          <span className="absolute inline-flex size-full animate-ping rounded-full bg-violet/50 motion-reduce:hidden" />
          <span className="relative inline-flex size-2 rounded-full bg-violet" />
        </span>
      ) : null}
      <div>
        <p className="font-medium text-ink">{view.title}</p>
        <p className="mt-1 text-[0.875rem] text-ink-2">{view.body}</p>
      </div>
    </div>
  );
}
