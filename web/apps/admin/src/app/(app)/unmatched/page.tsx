import { formatKes } from "@talksasa/money";
import { Amount } from "@talksasa/ui";
import type { Metadata } from "next";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { getMe, load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";

import { AssignForm } from "../transactions/[id]/AssignForm";
import { TxnTable } from "../transactions/TxnTable";

export const metadata: Metadata = { title: "Unmatched payments" };

const KEYS = ["receipt", "phone", "from", "to", "cursor"] as const;

export default async function Unmatched({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const [page, dash, apps, me] = await Promise.all([
    load((c) => c.GET("/transactions/unmatched", { params: { query: { ...p, limit: 50 } as QueryOf<"/transactions/unmatched"> } })),
    load((c) => c.GET("/dashboard")),
    load((c) => c.GET("/apps")),
    getMe(),
  ]);
  const canAssign = me.permissions.includes("payments.resolve");
  const n = dash.counts.unmatched_count;
  return (
    <>
      <PageHeader
        title="Unmatched payments"
        lead={
          <>
            Paybill payments whose account number starts with no app’s prefix, usually a typo. The money is recorded and safe:{" "}
            <Amount value={dash.counts.unmatched_amount} /> across {n} {n === 1 ? "payment" : "payments"}.{" "}
            {canAssign ? "Assign each one to the app it was meant for; the app is then told." : "Finance or an admin assigns them to apps."}
          </>
        }
      />
      <FilterBar action="/unmatched" active={anySet(p)}>
        <FilterField label="M-Pesa receipt">
          <input name="receipt" defaultValue={p.receipt} placeholder="SKL4ABCD12" className={`${filterInput} w-36 font-mono`} />
        </FilterField>
        <FilterField label="Phone">
          <input name="phone" defaultValue={p.phone} placeholder="0712 345 678" className={`${filterInput} w-36`} inputMode="tel" />
        </FilterField>
        <FilterField label="From">
          <input type="date" name="from" defaultValue={p.from} className={filterInput} />
        </FilterField>
        <FilterField label="To">
          <input type="date" name="to" defaultValue={p.to} className={filterInput} />
        </FilterField>
      </FilterBar>
      <Panel flush>
        <TxnTable
          items={page.items}
          empty={{ title: "Nothing unmatched", body: "Every payment has been routed to an app." }}
          action={
            canAssign
              ? (t) => <AssignForm compact id={t.id} apps={apps.items} amountLabel={formatKes(t.amount)} accountNumber={t.account_number} />
              : undefined
          }
        />
        <Pager basePath="/unmatched" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
