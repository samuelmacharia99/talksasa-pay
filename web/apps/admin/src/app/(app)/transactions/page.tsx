import type { Metadata } from "next";

import { AppSelect } from "@/components/AppSelect";
import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";

import { TxnTable } from "./TxnTable";

export const metadata: Metadata = { title: "Transactions" };

const KEYS = ["app_id", "charge_id", "status", "channel", "direction", "receipt", "phone", "from", "to", "unmatched", "cursor"] as const;

export default async function Transactions({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const [page, apps] = await Promise.all([
    load((c) => c.GET("/transactions", { params: { query: { ...p, limit: 50 } as QueryOf<"/transactions"> } })),
    load((c) => c.GET("/apps")),
  ]);
  return (
    <>
      <PageHeader title="Transactions" lead="Every shilling in and out, newest first. Rows are never edited or deleted; corrections appear as new rows." />
      <FilterBar action="/transactions" active={anySet(p)}>
        {p.charge_id ? <input type="hidden" name="charge_id" value={p.charge_id} /> : null}
        {p.unmatched ? <input type="hidden" name="unmatched" value={p.unmatched} /> : null}
        {p.direction ? <input type="hidden" name="direction" value={p.direction} /> : null}
        <FilterField label="App">
          <AppSelect apps={apps.items} value={p.app_id} />
        </FilterField>
        <FilterField label="Status">
          <select name="status" defaultValue={p.status ?? ""} className={filterInput}>
            <option value="">Any</option>
            <option value="succeeded">Succeeded</option>
            <option value="pending">Pending</option>
            <option value="needs_review">Needs review</option>
            <option value="failed">Failed</option>
            <option value="cancelled">Cancelled</option>
          </select>
        </FilterField>
        <FilterField label="Channel">
          <select name="channel" defaultValue={p.channel ?? ""} className={filterInput}>
            <option value="">Any</option>
            <option value="mpesa_stk">M-Pesa prompt</option>
            <option value="mpesa_c2b">M-Pesa paybill</option>
            <option value="mpesa_b2c">M-Pesa payout</option>
            <option value="manual">Manual</option>
          </select>
        </FilterField>
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
        <TxnTable items={page.items} empty={{ title: "No transactions match", body: "Try a wider date range or clear the filters." }} />
        <Pager basePath="/transactions" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
