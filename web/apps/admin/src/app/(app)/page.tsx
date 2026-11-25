import { formatKes } from "@talksasa/money";
import { Amount, BarChart, cx, DateTime } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";
import { Suspense } from "react";

import { Panel } from "@/components/PageHeader";
import { api, load } from "@/lib/api";
import { log } from "@/lib/log";
import { attentionItems, nairobiToday, overviewTiles } from "@/lib/overview";

export const metadata: Metadata = { title: "Overview" };

const UNASSIGNED = "00000000-0000-0000-0000-000000000000";

const FAILED_SAMPLE = 100;

const toneText = {
  ok: "text-ok",
  warn: "text-warn",
  bad: "text-bad",
  info: "text-ink",
  neutral: "text-ink",
  violet: "text-violet",
} as const;

/**
 * How many webhook deliveries have failed (capped: "100+"). Best effort: the
 * overview still renders if this lookup fails.
 */
async function failedDeliveries(): Promise<{ count: number; more: boolean } | null> {
  try {
    const c = await api();
    const { data } = await c.GET("/webhook-deliveries", { params: { query: { status: "failed", limit: FAILED_SAMPLE } } });
    return data ? { count: data.items.length, more: Boolean(data.next_cursor) } : null;
  } catch (e) {
    log.error({ err: e instanceof Error ? e.message : String(e) }, "could not count failed webhook deliveries");
    return null;
  }
}

export default async function Overview() {
  const [d, failed] = await Promise.all([load((c) => c.GET("/dashboard")), failedDeliveries()]);
  const names = new Map(d.apps.map((a) => [a.id, `${a.name} (${a.account_prefix})`]));
  names.set(UNASSIGNED, "Unmatched (no app yet)");
  const split = Object.entries(d.last_30_days.by_app).sort((a, b) => b[1] - a[1]);
  const tiles = overviewTiles(d.counts, nairobiToday(new Date(d.generated_at)));
  const attention = attentionItems(d.counts, failed);

  return (
    <div className="grid gap-8">
      <section aria-labelledby="today" className="pt-2">
        <h1 id="today" className="font-display text-[clamp(2rem,4.2vw,3.1rem)] leading-[1.08] font-semibold tracking-[-0.025em]">
          <span className="num">{formatKes(d.today.total)}</span> <span className="text-muted">collected today</span>
        </h1>
        <p className="mt-3 max-w-2xl text-[0.9375rem] text-ink-2">
          From {d.today.payments} {d.today.payments === 1 ? "payment" : "payments"}. <Amount value={d.last_7_days.total} /> over the
          last 7 days and <Amount value={d.last_30_days.total} /> over 30.
        </p>
        <p className="mt-1 text-[0.8125rem] text-faint">
          Kenyan time. Updated <DateTime value={d.generated_at} />.
        </p>
      </section>

      <ul aria-label="Counts" className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-line bg-line sm:grid-cols-3 xl:grid-cols-6">
        {tiles.map((t) => (
          <li key={t.label} className="bg-surface">
            <Link href={t.href} className="group block h-full px-4 py-3.5 hover:bg-wash">
              <span className="block text-[0.8125rem] text-muted group-hover:text-ink">{t.label}</span>
              <span className={cx("num mt-1 block font-display text-xl font-semibold tracking-[-0.01em]", toneText[t.tone])}>{t.value}</span>
              <span className="mt-0.5 block text-[0.75rem] leading-snug text-muted">{t.detail}</span>
            </Link>
          </li>
        ))}
      </ul>

      <div className="grid gap-6 lg:grid-cols-[1.6fr_1fr]">
        <Panel title="Daily collections, last 30 days">
          <BarChart data={d.daily} caption="Collections per day for the last 30 days, in Kenya shillings" />
        </Panel>
        <Panel title="By app, last 30 days" flush>
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
          {attention.length === 0 ? (
            <p className="p-5 text-sm text-muted">Nothing needs attention right now.</p>
          ) : (
            <ul className="divide-y divide-line">
              {attention.map((a) => (
                <li key={a.href}>
                  <Link href={a.href} className="group flex items-start gap-3 px-5 py-3.5 hover:bg-wash">
                    <span aria-hidden className={cx("mt-1.5 size-2 shrink-0 rounded-full", a.urgent ? "bg-bad" : "bg-warn")} />
                    <span className="min-w-0">
                      <span className="block text-sm font-medium text-ink group-hover:underline">{a.label}</span>
                      <span className="block text-[0.8125rem] text-muted">{a.detail}</span>
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Panel>
        <Suspense
          fallback={
            <Panel title="Ledger health">
              <p className="text-sm text-muted">Checking…</p>
            </Panel>
          }
        >
          <Health />
        </Suspense>
      </div>
    </div>
  );
}

async function Health() {
  const i = await load((c) => c.GET("/integrity"));
  return (
    <Panel title="Ledger health">
      <p className={cx("font-medium", i.audit_chain_ok ? "text-ok" : "text-bad")}>
        {i.audit_chain_ok ? "All checks pass" : "A check failed. Escalate now."}
      </p>
      <p className="mt-3 text-sm text-ink-2">
        {i.audit_chain_ok
          ? "Audit log is intact: no entry has been changed or removed."
          : `Audit log was altered at entry ${i.first_bad_audit_id}.`}
      </p>
      <p className="mt-3 text-[0.8125rem] text-faint">
        Checked <DateTime value={i.checked_at} />
      </p>
    </Panel>
  );
}
