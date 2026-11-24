import { formatKes } from "@talksasa/money";
import { Amount, BarChart, cx, DateTime } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";
import { Suspense } from "react";

import { Panel } from "@/components/PageHeader";
import { load } from "@/lib/api";

export const metadata: Metadata = { title: "Overview" };

const UNASSIGNED = "00000000-0000-0000-0000-000000000000";

export default async function Overview() {
  const d = await load((c) => c.GET("/dashboard"));
  const names = new Map(d.products.map((p) => [p.id, `${p.name} (${p.account_prefix})`]));
  names.set(UNASSIGNED, "Unmatched (not yet assigned)");
  const split = Object.entries(d.last_30_days.by_product).sort((a, b) => b[1] - a[1]);
  const c = d.counts;

  const attention: Array<{ href: string; label: string; detail: string; urgent: boolean }> = [
    {
      href: "/suspense",
      label: c.suspense_count === 1 ? "1 unmatched payment" : `${c.suspense_count} unmatched payments`,
      detail: c.suspense_count ? `${formatKes(c.suspense_amount)} waiting to be assigned to a payer` : "Every payment has found its payer",
      urgent: c.suspense_count > 0,
    },
    {
      href: "/transactions?status=needs_review",
      label: c.needs_review === 1 ? "1 transaction needs review" : `${c.needs_review} transactions need review`,
      detail: c.needs_review ? "M-Pesa did not confirm these in time; check before acting" : "Nothing is waiting on M-Pesa confirmation",
      urgent: c.needs_review > 0,
    },
    {
      href: "/transactions?status=pending",
      label: `${c.pending} pending`,
      detail: "Payments started but not yet confirmed by M-Pesa",
      urgent: false,
    },
    {
      href: "/requests?status=open",
      label: `${c.open_requests} unpaid bills`,
      detail: `${formatKes(c.outstanding)} outstanding across all products`,
      urgent: false,
    },
  ];

  return (
    <div className="grid gap-8">
      <section aria-labelledby="today" className="pt-2">
        <h1 id="today" className="font-display text-[clamp(2rem,4.2vw,3.1rem)] leading-[1.08] font-semibold tracking-[-0.025em]">
          <span className="num">{formatKes(d.today.total)}</span>{" "}
          <span className="text-muted">collected today</span>
        </h1>
        <p className="mt-3 max-w-2xl text-[0.9375rem] text-ink-2">
          From {d.today.payments} {d.today.payments === 1 ? "payment" : "payments"}.{" "}
          <Amount value={d.last_7_days.total} /> over the last 7 days and <Amount value={d.last_30_days.total} /> over 30.
          Customers hold <Amount value={c.credit_held} /> in credit.
        </p>
        <p className="mt-1 text-[0.8125rem] text-faint">
          Kenyan time. Updated <DateTime value={d.generated_at} />.
        </p>
      </section>

      <div className="grid gap-6 lg:grid-cols-[1.6fr_1fr]">
        <Panel title="Daily collections, last 30 days">
          <BarChart data={d.daily} caption="Collections per day for the last 30 days, in Kenya shillings" />
        </Panel>
        <Panel title="By product, last 30 days" flush>
          {split.length === 0 ? (
            <p className="p-5 text-sm text-muted">No payments in the last 30 days.</p>
          ) : (
            <ul className="divide-y divide-line">
              {split.map(([id, total]) => (
                <li key={id} className="px-5 py-3.5">
                  <div className="flex items-baseline justify-between gap-4 text-sm">
                    <span className={cx("min-w-0 truncate", id === UNASSIGNED && "text-warn")}>{names.get(id) ?? id}</span>
                    <Amount value={total} className="font-medium" />
                  </div>
                  <div className="mt-2 h-1 rounded-full bg-wash">
                    <div
                      className={cx("h-1 rounded-full", id === UNASSIGNED ? "bg-warn" : "bg-violet")}
                      style={{ width: `${Math.max(2, (total / Math.max(1, d.last_30_days.total)) * 100)}%` }}
                    />
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>

      <div className="grid gap-6 lg:grid-cols-[1.6fr_1fr]">
        <Panel title="Needs attention" flush>
          <ul className="divide-y divide-line">
            {attention.map((a) => (
              <li key={a.href}>
                <Link href={a.href} className="group flex items-start gap-3 px-5 py-3.5 hover:bg-wash">
                  <span aria-hidden className={cx("mt-1.5 size-2 shrink-0 rounded-full", a.urgent ? "bg-bad" : "bg-line-strong")} />
                  <span className="min-w-0">
                    <span className="block text-sm font-medium text-ink group-hover:underline">{a.label}</span>
                    <span className="block text-[0.8125rem] text-muted">{a.detail}</span>
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </Panel>
        <Suspense fallback={<Panel title="Ledger health"><p className="text-sm text-muted">Checking…</p></Panel>}>
          <Health />
        </Suspense>
      </div>
    </div>
  );
}

async function Health() {
  const i = await load((c) => c.GET("/integrity"));
  const ok = i.audit_chain_ok && i.ledger_ok;
  return (
    <Panel title="Ledger health">
      <p className={cx("font-medium", ok ? "text-ok" : "text-bad")}>{ok ? "All checks pass" : "A check failed. Escalate now."}</p>
      <ul className="mt-3 grid gap-2 text-sm text-ink-2">
        <li>
          {i.audit_chain_ok
            ? "Audit log is intact: no entry has been changed or removed."
            : `Audit log was altered at entry ${i.first_bad_audit_id}.`}
        </li>
        <li>
          {i.ledger_ok
            ? "Every bill and credit balance matches its payment records."
            : `${i.drift.length} balance(s) disagree with their payment records.`}
        </li>
      </ul>
      <p className="mt-3 text-[0.8125rem] text-faint">
        Checked <DateTime value={i.checked_at} />
      </p>
    </Panel>
  );
}
