import { Amount, DateTime, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { load } from "@/lib/api";
import { actorLabel, BATCH_KIND, CHANNEL, KIND } from "@/lib/labels";

export const metadata: Metadata = { title: "Transaction" };

export default async function TransactionPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const t = await load((c) => c.GET("/transactions/{id}", { params: { path: { id } } }));
  const out = t.direction === "out";
  return (
    <>
      <PageHeader
        back={<Link href="/transactions" className="text-violet hover:underline">Transactions</Link>}
        title={
          <>
            <Amount value={out ? -t.amount : t.amount} /> <span className="text-muted">{out ? "paid out" : "received"}</span>
          </>
        }
        lead={
          <>
            {KIND[t.kind]} by {CHANNEL[t.channel] ?? t.channel}
            {t.receipt ? (
              <>
                , receipt <span className="font-mono text-ink">{t.receipt}</span>
              </>
            ) : null}
            .
          </>
        }
        actions={<Status value={t.status} className="text-sm" />}
      />
      <div className="grid gap-6">
        <Panel title="Details">
          <Facts
            items={[
              ["Product", t.account_prefix ?? <span className="text-warn">Not matched</span>],
              [
                "Payer",
                t.payer_account_id ? (
                  <Link href={`/payers/${t.payer_account_id}`} className="text-violet hover:underline">
                    {t.payer_name} ({t.account_number})
                  </Link>
                ) : (
                  <span className="text-warn">No payer matched</span>
                ),
              ],
              ["Account number as typed", t.bill_ref ?? "—"],
              [
                "Started for bill",
                t.payment_request_id ? (
                  <Link href={`/requests/${t.payment_request_id}`} className="text-violet hover:underline">
                    {t.product_reference}
                  </Link>
                ) : (
                  "No specific bill"
                ),
              ],
              ["Phone", t.phone ?? "—"],
              ["M-Pesa request id", t.provider_request_id ? <span className="font-mono text-[0.8125rem]">{t.provider_request_id}</span> : "—"],
              ["Recorded", <DateTime key="c" value={t.created_at} />],
              ["Completed", <DateTime key="d" value={t.completed_at} fallback="Not yet" />],
              ["M-Pesa time", <DateTime key="o" value={t.occurred_at} />],
              ...(t.reason ? ([["Reason", t.reason]] as Array<[string, string]>) : []),
              ...(t.created_by ? ([["Done by", actorLabel(t.created_by)]] as Array<[string, string]>) : []),
              ...(t.failure_reason ? ([["Failure", `${t.failure_reason}${t.failure_code ? ` (code ${t.failure_code})` : ""}`]] as Array<[string, string]>) : []),
              ...(t.reverses_transaction_id
                ? ([["Reverses", <Link key="r" href={`/transactions/${t.reverses_transaction_id}`} className="text-violet hover:underline">original payment</Link>]] as Array<[string, React.ReactNode]>)
                : []),
            ]}
          />
        </Panel>

        <Panel title="Where the money went" flush>
          {t.allocations.length === 0 ? (
            <p className="p-5 text-sm text-muted">
              {t.payer_account_id ? "Not applied to any bill yet." : "Waiting to be matched to a payer before it can be applied."}
            </p>
          ) : (
            <Table label="Allocations">
              <thead>
                <tr>
                  <Th>Step</Th>
                  <Th>Applied to</Th>
                  <Th>When</Th>
                  <Th align="right">Amount</Th>
                </tr>
              </thead>
              <tbody>
                {t.allocations.map((a) => (
                  <tr key={a.id}>
                    <Td className="text-ink-2">{BATCH_KIND[a.batch_kind]}</Td>
                    <Td>
                      {a.payment_request_id ? (
                        <Link href={`/requests/${a.payment_request_id}`} className="text-violet hover:underline">
                          Bill {a.product_reference}
                        </Link>
                      ) : (
                        "Customer credit"
                      )}
                    </Td>
                    <Td>
                      <DateTime value={a.created_at} />
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

        <Panel title="Status history">
          <ol className="grid gap-3">
            {t.events.map((e, i) => (
              <li key={i} className="flex flex-wrap items-baseline gap-x-3 text-sm">
                <DateTime value={e.occurred_at} />
                <Status value={e.to_status} />
                <span className="text-muted">{e.from_status ? `from ${e.from_status.replace("_", " ")}` : "recorded"}, by {actorLabel(e.actor)}</span>
              </li>
            ))}
          </ol>
        </Panel>

        {t.raw_payload ? (
          <Panel title="Message from M-Pesa">
            <p className="mb-3 text-[0.8125rem] text-muted">As received, with phone numbers masked.</p>
            <pre className="overflow-x-auto rounded-md bg-wash p-4 font-mono text-[0.75rem] leading-relaxed text-ink-2">
              {JSON.stringify(t.raw_payload, null, 2)}
            </pre>
          </Panel>
        ) : null}
      </div>
    </>
  );
}
