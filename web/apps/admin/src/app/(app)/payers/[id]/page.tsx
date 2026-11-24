import { Amount, DateTime, Status } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { load } from "@/lib/api";
import { BATCH_KIND } from "@/lib/labels";

import { RequestTable } from "../../requests/RequestTable";
import { TxnTable } from "../../transactions/TxnTable";

export const metadata: Metadata = { title: "Payer" };

export default async function PayerPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const [py, requests, txns] = await Promise.all([
    load((c) => c.GET("/payers/{id}", { params: { path: { id } } })),
    load((c) => c.GET("/payment-requests", { params: { query: { payer_id: id, limit: 20 } } })),
    load((c) => c.GET("/transactions", { params: { query: { payer_id: id, limit: 20 } } })),
  ]);
  return (
    <>
      <PageHeader
        back={<Link href="/payers" className="text-violet hover:underline">Payers</Link>}
        title={py.display_name}
        lead={
          <>
            Account <span className="font-medium text-ink">{py.account_number}</span> with {py.product_name}
          </>
        }
        actions={<Status value={py.status} className="text-sm" />}
      />
      <div className="grid gap-6">
        <Panel title="Details">
          <Facts
            items={[
              ["Credit balance", <Amount key="c" value={py.credit_balance} className="font-medium" />],
              ["Customer ref in product", py.external_ref],
              ["Email", py.email ?? "—"],
              [
                "Phones",
                py.phones.length ? py.phones.map((ph) => `${ph.phone}${ph.is_primary ? " (primary)" : ""}`).join(", ") : "—",
              ],
              ["Payer since", <DateTime key="s" value={py.created_at} dateOnly />],
              ["Last updated", <DateTime key="u" value={py.updated_at} />],
            ]}
          />
        </Panel>
        <Panel
          title="Bills"
          actions={
            <Link href={`/requests?payer_id=${py.id}`} className="text-sm text-violet hover:underline">
              All bills
            </Link>
          }
          flush
        >
          <RequestTable items={requests.items} showPayer={false} />
        </Panel>
        <Panel
          title="Payments"
          actions={
            <Link href={`/transactions?payer_id=${py.id}`} className="text-sm text-violet hover:underline">
              All payments
            </Link>
          }
          flush
        >
          <TxnTable items={txns.items} empty={{ title: "No payments yet", body: "Payments to this account number will appear here." }} />
        </Panel>
        {py.credit_movements.length > 0 ? (
          <Panel title="Credit movements">
            <ul className="grid gap-2.5 text-sm">
              {py.credit_movements.map((m) => (
                <li key={m.id} className="flex flex-wrap items-baseline justify-between gap-3">
                  <span className="text-ink-2">
                    <DateTime value={m.created_at} />{" "}
                    {m.amount > 0 ? "Overpayment kept as credit" : `${BATCH_KIND[m.batch_kind]}`}
                  </span>
                  <Amount value={m.amount} signed />
                </li>
              ))}
            </ul>
          </Panel>
        ) : null}
      </div>
    </>
  );
}
