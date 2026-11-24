import type { Metadata } from "next";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { ProductSelect } from "@/components/ProductSelect";
import { load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";

import { RequestTable } from "./RequestTable";

export const metadata: Metadata = { title: "Payment requests" };

const KEYS = ["product_id", "status", "q", "payer_id", "cursor"] as const;

export default async function Requests({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const [page, products] = await Promise.all([
    load((c) => c.GET("/payment-requests", { params: { query: { ...p, limit: 50 } as QueryOf<"/payment-requests"> } })),
    load((c) => c.GET("/products")),
  ]);
  return (
    <>
      <PageHeader title="Payment requests" lead="Bills that products have asked their customers to pay. Amounts are in Kenya shillings." />
      <FilterBar action="/requests" active={anySet(p)}>
        <FilterField label="Search">
          <input name="q" defaultValue={p.q} placeholder="Reference, description or account" className={`${filterInput} w-64`} />
        </FilterField>
        <FilterField label="Product">
          <ProductSelect products={products.items} value={p.product_id} />
        </FilterField>
        <FilterField label="Status">
          <select name="status" defaultValue={p.status ?? ""} className={filterInput}>
            <option value="">Any</option>
            <option value="open">Open</option>
            <option value="partially_paid">Part paid</option>
            <option value="paid">Paid</option>
            <option value="cancelled">Cancelled</option>
            <option value="expired">Expired</option>
          </select>
        </FilterField>
      </FilterBar>
      <Panel flush>
        <RequestTable items={page.items} />
        <Pager basePath="/requests" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
