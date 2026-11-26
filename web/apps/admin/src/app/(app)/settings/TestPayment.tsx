"use client";

import { formatKes } from "@talksasa/money";
import { Field, formatDateTime } from "@talksasa/ui";
import Link from "next/link";
import { useActionState, useEffect, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { ToneLabel } from "@/components/ToneLabel";
import { CONFIRM_WORD } from "@/lib/settings";
import { checkTestPayment, TEST_MAX, TEST_MIN, testOutcome } from "@/lib/testpay";

import { recentTestPayments, startTestPayment, type TestPayment } from "./actions";

const POLL_MS = 3000;
// Safaricom gives up on a prompt after about a minute; Talksasa Pay then
// asks for the status. Stop polling after that, the list still updates on reload.
const POLL_FOR_MS = 3 * 60_000;

function newKey(): string {
  return crypto.randomUUID();
}

/**
 * Sends a real KES 1-1,000 M-Pesa prompt and follows it until Safaricom
 * answers. The money is recorded under the internal Gateway test app.
 */
export function TestPaymentPanel({
  production,
  ready,
  initial,
  initialKey,
}: {
  production: boolean;
  ready: boolean;
  initial: TestPayment[];
  /** Made on the server so the first render matches; later keys are made here. */
  initialKey: string;
}) {
  const [state, action] = useActionState(startTestPayment, undefined);
  const [key, setKey] = useState(initialKey);
  const [items, setItems] = useState(initial);
  const [clientError, setClientError] = useState<{ field: string; message: string } | undefined>();
  const [watching, setWatching] = useState<string | null>(null);
  const [handled, setHandled] = useState<typeof state>(undefined);

  // A started test: show it at the top, follow it, and give the form a new key.
  if (state !== handled) {
    setHandled(state);
    if (state?.ok) {
      const t = state.data;
      setItems((cur) => [t, ...cur.filter((x) => x.charge_id !== t.charge_id)]);
      setWatching(t.charge_id);
      setKey(newKey());
    }
  }

  const current = watching ? items.find((x) => x.charge_id === watching) : undefined;
  const live = current ? !testOutcome(current).final : false;

  useEffect(() => {
    if (!watching || !live) return;
    const until = Date.now() + POLL_FOR_MS;
    const timer = setInterval(async () => {
      if (Date.now() > until) {
        clearInterval(timer);
        setWatching(null);
        return;
      }
      const fresh = await recentTestPayments();
      if (fresh.length) setItems(fresh);
    }, POLL_MS);
    return () => clearInterval(timer);
  }, [watching, live]);

  const err = (f: string) =>
    clientError?.field === f
      ? clientError.message
      : state && !state.ok
        ? (state.fieldErrors?.find((e) => e.field === f)?.message ?? (state.field === f ? state.error : undefined))
        : undefined;
  const formError = state && !state.ok && !state.field && !state.fieldErrors?.length ? state.error : undefined;

  return (
    <div className="grid gap-5">
      <p className="text-sm text-ink-2">
        Sends a real M-Pesa prompt for KES {TEST_MIN} to {formatKes(TEST_MAX)} to the phone you enter, and follows it through the same steps as an app&apos;s
        charge: prompt, Safaricom&apos;s answer, receipt and ledger.{" "}
        {production ? (
          <strong className="font-medium text-ink">This is the production paybill: the money is real.</strong>
        ) : (
          "In the sandbox no real money moves."
        )}{" "}
        It is recorded under the internal <span className="font-medium">Gateway test</span> app, never a real app. Refunds are made by hand in the M-Pesa portal.
      </p>

      {!ready ? (
        <p role="status" className="rounded-md border border-warn/30 bg-warn-soft px-3 py-2.5 text-sm text-warn">
          Save complete paybill settings before sending a test payment.
        </p>
      ) : (
        <form
          action={action}
          onSubmit={(e) => {
            const fd = new FormData(e.currentTarget);
            const problem = checkTestPayment({ amount: String(fd.get("amount") ?? ""), phone: String(fd.get("phone") ?? "") });
            setClientError(problem);
            if (problem) e.preventDefault();
          }}
          className="grid gap-4"
          autoComplete="off"
          aria-label="Test payment"
        >
          <FormError message={formError ?? err("key")} />
          <input type="hidden" name="key" value={key} />
          <div className="grid gap-4 sm:grid-cols-[10rem_minmax(0,16rem)]">
            <Field
              label="Amount (KES)"
              name="amount"
              type="number"
              inputMode="numeric"
              min={TEST_MIN}
              max={TEST_MAX}
              step={1}
              required
              defaultValue="1"
              error={err("amount")}
              className="content-start"
            />
            <Field
              label="Phone number"
              name="phone"
              type="tel"
              inputMode="tel"
              placeholder="0712 345 678"
              maxLength={20}
              required
              hint="The Safaricom line that will get the prompt."
              error={err("phone")}
              className="content-start"
            />
          </div>
          {production ? (
            <Field
              label={`Type "${CONFIRM_WORD}" to confirm`}
              name="confirm"
              required
              pattern={CONFIRM_WORD}
              autoComplete="off"
              hint="Real money will be taken from this phone's M-Pesa."
              error={err("confirm")}
              className="max-w-xs"
            />
          ) : null}
          <div>
            <SubmitButton pending="Sending…">Send test payment</SubmitButton>
          </div>
        </form>
      )}

      {items.length ? (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[36rem] text-sm" aria-label="Recent test payments">
            <thead>
              <tr className="border-b border-line text-left text-[0.8125rem] text-muted">
                <th className="py-2 pr-4 font-medium">Sent</th>
                <th className="py-2 pr-4 font-medium">Amount</th>
                <th className="py-2 pr-4 font-medium">Phone</th>
                <th className="py-2 pr-4 font-medium">Result</th>
                <th className="py-2 font-medium">
                  <span className="sr-only">Charge</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((t) => {
                const o = testOutcome(t);
                const following = watching === t.charge_id && !o.final;
                return (
                  <tr key={t.charge_id} className="border-b border-line align-top last:border-0" data-testid="test-payment">
                    <td className="py-2.5 pr-4 whitespace-nowrap text-muted">{formatDateTime(t.created_at)}</td>
                    <td className="num py-2.5 pr-4 whitespace-nowrap">{formatKes(t.amount)}</td>
                    <td className="py-2.5 pr-4 font-mono text-[0.8125rem] whitespace-nowrap">{t.push?.phone ?? "—"}</td>
                    <td className="py-2.5 pr-4" aria-live={following ? "polite" : undefined}>
                      <ToneLabel label={following ? `${o.label}…` : o.label} tone={o.tone} />
                      {o.detail ? <p className="mt-0.5 text-[0.8125rem] text-muted">{o.detail}</p> : null}
                    </td>
                    <td className="py-2.5 text-right whitespace-nowrap">
                      <Link href={`/charges/${t.charge_id}`} className="text-violet hover:underline">
                        Charge
                      </Link>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}
