import { formatKes } from "@talksasa/money";
import { Amount, CopyButton, formatDate } from "@talksasa/ui";
import type { Metadata } from "next";
import { notFound } from "next/navigation";

import { getPayPage } from "@/lib/pay";

import { Slip } from "../Slip";
import { LiveProvider, LiveStatus } from "./LiveStatus";
import { StkPanel } from "./StkPanel";

export const metadata: Metadata = { title: "Pay your bill", referrer: "no-referrer" };

export default async function PayPage({ params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;
  const res = await getPayPage(token);
  if (!res.found) notFound();
  const b = res.page;

  const header = (
    <>
      <p className="text-sm text-muted">{b.product_name}</p>
      <h1 className="mt-1 font-display text-lg leading-snug font-semibold">
        {b.description}
      </h1>
      <p className="mt-1 text-sm text-ink-2">Bill {b.reference}</p>
    </>
  );

  if (b.status === "paid") {
    return (
      <Slip
        top={
          <>
            {header}
            <div className="mt-7 flex items-center gap-3">
              <span aria-hidden className="grid size-10 place-items-center rounded-full bg-ok-soft text-ok">
                <svg viewBox="0 0 20 20" className="size-5" fill="none" stroke="currentColor" strokeWidth={2.4}>
                  <path d="M4.5 10.5l3.5 3.5 7.5-8" strokeLinecap="round" strokeLinejoin="round" />
                </svg>
              </span>
              <p className="font-display text-[2rem] leading-none font-semibold tracking-[-0.02em]">Paid</p>
            </div>
            <p className="mt-4 text-[0.9375rem] text-ink-2">
              Thank you. We received <Amount value={b.amount_paid} className="font-medium text-ink" />
              {b.paid_at ? ` on ${formatDate(b.paid_at)}` : ""}. There is nothing more to pay on this bill.
            </p>
          </>
        }
      />
    );
  }

  if (!b.payable) {
    return (
      <Slip
        top={
          <>
            {header}
            <p className="mt-7 font-display text-2xl font-semibold tracking-[-0.015em]">
              {b.status === "cancelled" ? "This bill was cancelled" : "This bill has expired"}
            </p>
            <p className="mt-3 text-[0.9375rem] text-ink-2">
              It can no longer be paid. If you still owe {b.product_name}, ask them for a new payment link. If you already paid,
              your money is safe: keep your M-Pesa confirmation and contact {b.product_name}.
            </p>
          </>
        }
      />
    );
  }

  const showStk = b.stk_available || b.stk_push !== null;
  const initialStk = b.stk_push ? { status: b.stk_push.status, message: b.stk_push.message } : null;

  return (
    <LiveProvider token={token} status={b.status} amountPaid={b.amount_paid} initialStk={initialStk}>
      <Slip
        top={
          <>
            {header}
            <p className="mt-7 text-sm text-muted">{b.amount_paid > 0 ? "Left to pay" : "Amount due"}</p>
            <p className="num font-display text-[2.6rem] leading-[1.05] font-semibold tracking-[-0.03em]">
              {formatKes(b.outstanding)}
            </p>
            {b.amount_paid > 0 ? (
              <p className="mt-2 text-sm text-ink-2">
                <Amount value={b.amount_paid} /> of <Amount value={b.amount} /> already received.
              </p>
            ) : null}
            {b.expires_at ? <p className="mt-2 text-sm text-ink-2">Pay by {formatDate(b.expires_at)}.</p> : null}
            <LiveStatus />
          </>
        }
        bottom={
          b.paybill ? (
            <>
              {showStk ? <StkPanel token={token} amountLabel={formatKes(b.outstanding)} canSend={b.stk_available} /> : null}
              <h2 className="font-display text-base font-semibold">{showStk ? "Or pay with Paybill" : "Pay with M-Pesa"}</h2>
              <ol className="mt-4 grid gap-4 text-[0.9375rem]">
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={1} />
                  <span>
                    On your phone open <span className="font-medium">M-Pesa</span>, then{" "}
                    <span className="font-medium">Lipa na M-Pesa</span>, then <span className="font-medium">Pay Bill</span>.
                  </span>
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={2} />
                  <Copyable label="Business number" value={b.paybill.business_number} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={3} />
                  <Copyable label="Account number" value={b.paybill.account_number} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={4} />
                  <Copyable label="Amount" value={String(b.outstanding)} display={formatKes(b.outstanding)} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={5} />
                  <span>Enter your M-Pesa PIN and confirm. You’ll get an SMS from M-Pesa.</span>
                </li>
              </ol>
              <p className="mt-5 text-[0.8125rem] text-muted">
                Paying less is fine: the rest stays on this bill. Paying more is kept as credit on your account.
              </p>
            </>
          ) : (
            <p className="text-[0.9375rem] text-ink-2">Online payment instructions aren’t available right now. Contact {b.product_name} to pay.</p>
          )
        }
      />
    </LiveProvider>
  );
}

function Step({ n }: { n: number }) {
  return (
    <span aria-hidden className="num mt-0.5 grid size-6 place-items-center rounded-full bg-violet-soft text-[0.75rem] font-semibold text-violet">
      {n}
    </span>
  );
}

function Copyable({ label, value, display }: { label: string; value: string; display?: string }) {
  return (
    <span className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
      <span>
        <span className="block text-[0.8125rem] text-muted">{label}</span>
        <span className="num block font-display text-xl font-semibold tracking-[-0.01em]">{display ?? value}</span>
      </span>
      <CopyButton value={value} label={`Copy`} />
    </span>
  );
}
