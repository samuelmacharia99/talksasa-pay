import { Amount, DateTime, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { load } from "@/lib/api";
import { BATCH_KIND } from "@/lib/labels";

export const metadata: Metadata = { title: "Payment request" };

export default async function RequestPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const r = await load((c) => c.GET("/payment-requests/{id}", { params: { path: { id } } }));
  const pct = Math.min(100, Math.round((r.amount_paid / r.amount) * 100));
  return (
    <>
      <PageHeader
        back={<Link href="/requests" className="text-violet hover:underline">Payment requests</Link>}
        title={`Bill ${r.product_reference}`}
        lead={r.description}
        actions={<Status value={r.status} className="text-sm" />}
      />
      <div className="grid gap-6">
        <Panel>
          <div className="flex flex-wrap items-end justify-between gap-6">
            <div>
              <p className="text-sm text-muted">Paid so far</p>
              <p className="font-display text-3xl font-semibold tracking-[-0.02em]">
                <Amount value={r.amount_paid} /> <span className="text-lg text-muted">of <Amount value={r.amount} /></span>
              </p>
            </div>
            {r.outstanding > 0 ? (
              <p className="text-sm text-ink-2">
                <Amount value={r.outstanding} className="font-medium text-ink" /> still to pay
              </p>
            ) : null}
          </div>
          <div className="mt-4 h-1.5 rounded-full bg-wash" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="Share paid">
            <div className="h-1.5 rounded-full bg-violet" style={{ width: `${pct}%` }} />
          </div>
        </Panel>
        <Panel title="Details">
          <Facts
            items={[
              ["Product", `${r.product_name} (${r.account_prefix})`],
              [
                "Payer",
                <Link key="p" href={`/payers/${r.payer_account_id}`} className="text-violet hover:underline">
                  {r.payer_name} ({r.account_number})
                </Link>,
              ],
              ["Created", <DateTime key="c" value={r.created_at} />],
              ["Expires", <DateTime key="e" value={r.expires_at} fallback="Never" />],
              ["Fully paid", <DateTime key="p2" value={r.paid_at} fallback="Not yet" />],
              ["Closed", <DateTime key="cl" value={r.closed_at} fallback="—" />],
            ]}
          />
        </Panel>
        <Panel title="Payments applied" flush>
          {r.allocations.length === 0 ? (
            <p className="p-5 text-sm text-muted">Nothing paid yet.</p>
          ) : (
            <Table label="Payments applied to this bill">
              <thead>
                <tr>
                  <Th>When</Th>
                  <Th>From</Th>
                  <Th>Receipt</Th>
                  <Th align="right">Amount</Th>
                </tr>
              </thead>
              <tbody>
                {r.allocations.map((a) => (
                  <tr key={a.id}>
                    <Td>
                      <DateTime value={a.created_at} />
                    </Td>
                    <Td className="text-ink-2">{BATCH_KIND[a.batch_kind]}</Td>
                    <Td className="font-mono text-[0.8125rem]">
                      {a.transaction_id ? (
                        <Link href={`/transactions/${a.transaction_id}`} className="text-violet hover:underline">
                          {a.receipt ?? "view"}
                        </Link>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </Td>
                    <Td align="right">
                      <Amount value={a.amount} signed />
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Panel>
      </div>
    </>
  );
}
