import { formatKes } from "@talksasa/money";
import { Amount, CopyButton, formatDate, formatDateTime } from "@talksasa/ui";
import type { Metadata } from "next";
import { notFound } from "next/navigation";
import type { ReactNode } from "react";

import { getPayPage } from "@/lib/pay";

import { Slip } from "../Slip";
import { LiveProvider, LiveStatus } from "./LiveStatus";
import { StkPanel } from "./StkPanel";

export const metadata: Metadata = { title: "Pay", referrer: "no-referrer" };

/**
 * The hosted checkout page for one charge: who is collecting, what for and
 * how much, then a phone prompt and the paybill steps while it can be paid.
 */
export default async function PayPage({ params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;
  const res = await getPayPage(token);
  if (!res.found) notFound();
  const c = res.page;

  const header = (
    <>
      <p className="text-sm text-muted">{c.app_name}</p>
      {c.description ? <h1 className="mt-1 font-display text-lg leading-snug font-semibold">{c.description}</h1> : null}
      <p className={c.description ? "mt-1 text-sm text-ink-2" : "mt-1 font-display text-lg leading-snug font-semibold"}>
        {c.description ? "Reference " : "Payment for "}
        {c.reference}
      </p>
    </>
  );

  if (c.status === "succeeded") {
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
              Thank you. {c.app_name} received <Amount value={c.amount} className="font-medium text-ink" />
              {c.succeeded_at ? ` on ${formatDate(c.succeeded_at)}` : ""}. There is nothing more to pay.
            </p>
          </>
        }
        bottom={
          <dl className="grid gap-3 text-[0.9375rem]">
            <Row label="Paid to">{c.app_name}</Row>
            <Row label="Reference">{c.reference}</Row>
            <Row label="Amount">
              <Amount value={c.amount} />
            </Row>
            {c.succeeded_at ? <Row label="Date">{formatDateTime(c.succeeded_at)}</Row> : null}
          </dl>
        }
      />
    );
  }

  if (c.status === "needs_review") {
    // Safaricom said paid but sent no receipt; staff confirm it. Keep polling
    // so the page turns to Paid (or back to payable) by itself.
    return (
      <LiveProvider token={token} status={c.status} initialStk={null}>
        <Slip
          top={
            <>
              {header}
              <p className="mt-7 text-sm text-muted">Amount</p>
              <p className="num font-display text-[2.6rem] leading-[1.05] font-semibold tracking-[-0.03em]">{formatKes(c.amount)}</p>
              <p className="mt-6 font-display text-xl font-semibold tracking-[-0.01em]">We’re confirming your payment</p>
              <p className="mt-2 text-[0.9375rem] text-ink-2">
                M-Pesa told us the payment went through, and we’re checking it with Safaricom. This usually takes a few minutes.
                Please don’t pay again: keep your M-Pesa confirmation SMS.
              </p>
              <LiveStatus />
            </>
          }
        />
      </LiveProvider>
    );
  }

  if (!c.payable) {
    const closed: Record<string, { title: string; body: ReactNode }> = {
      cancelled: {
        title: "This payment was cancelled",
        body: (
          <>
            It can no longer be paid. If you still need to pay {c.app_name}, ask them for a new payment link. If you already paid, your
            money is safe: keep your M-Pesa confirmation and contact {c.app_name}.
          </>
        ),
      },
      reversed: {
        title: "This payment was reversed",
        body: <>The money for it was returned. If you have questions, contact {c.app_name}.</>,
      },
    };
    const s = closed[c.status] ?? {
      title: "This payment link has expired",
      body: (
        <>
          It can no longer be paid. If you still need to pay {c.app_name}, ask them for a new payment link. If you already paid, your
          money is safe: keep your M-Pesa confirmation and contact {c.app_name}.
        </>
      ),
    };
    return (
      <Slip
        top={
          <>
            {header}
            <p className="mt-7 font-display text-2xl font-semibold tracking-[-0.015em]">{s.title}</p>
            <p className="mt-3 text-[0.9375rem] text-ink-2">{s.body}</p>
          </>
        }
      />
    );
  }

  const showStk = c.stk_available || c.stk_push !== null;
  const initialStk = c.stk_push ? { status: c.stk_push.status, message: c.stk_push.message } : null;

  return (
    <LiveProvider token={token} status={c.status} initialStk={initialStk}>
      <Slip
        top={
          <>
            {header}
            <p className="mt-7 text-sm text-muted">Amount to pay</p>
            <p className="num font-display text-[2.6rem] leading-[1.05] font-semibold tracking-[-0.03em]">{formatKes(c.amount)}</p>
            <p className="mt-2 text-sm text-ink-2">Pay by {formatDate(c.expires_at)}.</p>
            <LiveStatus />
          </>
        }
        bottom={
          c.paybill ? (
            <>
              {showStk ? <StkPanel token={token} amountLabel={formatKes(c.amount)} canSend={c.stk_available} /> : null}
              <h2 className="font-display text-base font-semibold">{showStk ? "Or pay with Paybill" : "Pay with M-Pesa"}</h2>
              <ol className="mt-4 grid gap-4 text-[0.9375rem]">
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={1} />
                  <span>
                    On your phone open <span className="font-medium">M-Pesa</span>, then <span className="font-medium">Lipa na M-Pesa</span>,
                    then <span className="font-medium">Pay Bill</span>.
                  </span>
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={2} />
                  <Copyable label="Business number" value={c.paybill.business_number} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={3} />
                  <Copyable label="Account number" value={c.paybill.account_number} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={4} />
                  <Copyable label="Amount" value={String(c.amount)} display={formatKes(c.amount)} />
                </li>
                <li className="grid grid-cols-[1.5rem_1fr] gap-2">
                  <Step n={5} />
                  <span>Enter your M-Pesa PIN and confirm. You’ll get an SMS from M-Pesa.</span>
                </li>
              </ol>
              <p className="mt-5 text-[0.8125rem] text-muted">
                Pay exactly {formatKes(c.amount)} to this account number. A different amount isn’t matched to this payment: {c.app_name}{" "}
                receives it and sorts it out with you.
              </p>
            </>
          ) : (
            <p className="text-[0.9375rem] text-ink-2">Online payment instructions aren’t available right now. Contact {c.app_name} to pay.</p>
          )
        }
      />
    </LiveProvider>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="text-muted">{label}</dt>
      <dd className="text-right font-medium text-ink">{children}</dd>
    </div>
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
      <CopyButton value={value} label="Copy" />
    </span>
  );
}
