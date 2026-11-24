import { Amount, DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { ProductSelect } from "@/components/ProductSelect";
import { load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";

export const metadata: Metadata = { title: "Payers" };

export default async function Payers({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, ["product_id", "q", "cursor"] as const);
  const [page, products] = await Promise.all([
    load((c) => c.GET("/payers", { params: { query: { ...p, limit: 50 } as QueryOf<"/payers"> } })),
    load((c) => c.GET("/products")),
  ]);
  return (
    <>
      <PageHeader title="Payers" lead="Customers of each product. The account number is what they type on the M-Pesa paybill menu." />
      <FilterBar action="/payers" active={anySet(p)}>
        <FilterField label="Search">
          <input
            name="q"
            defaultValue={p.q}
            placeholder="Account number, name, phone or customer ref"
            className={`${filterInput} w-80`}
            autoFocus={!p.q}
          />
        </FilterField>
        <FilterField label="Product">
          <ProductSelect products={products.items} value={p.product_id} />
        </FilterField>
      </FilterBar>
      <Panel flush>
        {page.items.length === 0 ? (
          <Empty title={p.q ? "No payer matches that search" : "No payers yet"}>
            {p.q ? "Account numbers match with or without the hyphen, in any case." : "Products register payers through the API."}
          </Empty>
        ) : (
          <Table label="Payers">
            <thead>
              <tr>
                <Th>Account number</Th>
                <Th>Name</Th>
                <Th>Customer ref</Th>
                <Th>Status</Th>
                <Th>Since</Th>
                <Th align="right">Credit</Th>
              </tr>
            </thead>
            <tbody>
              {page.items.map((py) => (
                <tr key={py.id} className="hover:bg-wash/60">
                  <Td>
                    <Link href={`/payers/${py.id}`} className="font-medium text-ink hover:text-violet hover:underline">
                      {py.account_number}
                    </Link>
                  </Td>
                  <Td>{py.display_name}</Td>
                  <Td className="text-ink-2">{py.external_ref}</Td>
                  <Td>
                    <Status value={py.status} />
                  </Td>
                  <Td>
                    <DateTime value={py.created_at} dateOnly />
                  </Td>
                  <Td align="right">
                    {py.credit_balance > 0 ? <Amount value={py.credit_balance} bare /> : <span className="text-faint">—</span>}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        <Pager basePath="/payers" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
