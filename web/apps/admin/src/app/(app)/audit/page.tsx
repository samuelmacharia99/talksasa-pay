import { DateTime, Empty, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import { Suspense } from "react";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { ProductSelect } from "@/components/ProductSelect";
import { load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";

export const metadata: Metadata = { title: "Audit log" };

const ACTIONS: Array<[string, string]> = [
  ["", "Everything"],
  ["admin.", "Sign-ins and sign-outs"],
  ["admin_user.", "Staff accounts"],
  ["product.", "Products"],
  ["api_key.", "API keys"],
  ["payer.", "Payers"],
  ["transaction.", "Transactions"],
];

const WHO: Record<string, string> = { admin: "Staff", cli: "Command line", product: "Product API", system: "System" };

export default async function Audit({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, ["product_id", "action", "cursor"] as const);
  const [page, products] = await Promise.all([
    load((c) => c.GET("/audit", { params: { query: { ...p, limit: 50 } as QueryOf<"/audit"> } })),
    load((c) => c.GET("/products")),
  ]);
  return (
    <>
      <PageHeader
        title="Audit log"
        lead="Who did what, and when. Each entry is chained to the one before it, so any edit or deletion is detected."
      />
      <Suspense fallback={null}>
        <ChainStatus />
      </Suspense>
      <FilterBar action="/audit" active={anySet(p)}>
        <FilterField label="Show">
          <select name="action" defaultValue={p.action ?? ""} className={filterInput}>
            {ACTIONS.map(([v, l]) => (
              <option key={v} value={v}>
                {l}
              </option>
            ))}
          </select>
        </FilterField>
        <FilterField label="Product">
          <ProductSelect products={products.items} value={p.product_id} />
        </FilterField>
      </FilterBar>
      <Panel flush>
        {page.items.length === 0 ? (
          <Empty title="No entries match" />
        ) : (
          <Table label="Audit log">
            <thead>
              <tr>
                <Th>When</Th>
                <Th>Who</Th>
                <Th>What</Th>
                <Th>Details</Th>
              </tr>
            </thead>
            <tbody>
              {page.items.map((a) => (
                <tr key={a.id} className="align-top">
                  <Td>
                    <DateTime value={a.occurred_at} />
                  </Td>
                  <Td>
                    <span className="block text-ink">{a.actor_name || (a.actor_type === "admin" ? "Staff member" : a.actor_id)}</span>
                    <span className="block text-[0.75rem] text-muted">{WHO[a.actor_type]}</span>
                  </Td>
                  <Td>
                    <span className="block font-medium">{a.action}</span>
                    <span className="block text-[0.75rem] text-muted">
                      {a.account_prefix ? `${a.account_prefix}, ` : ""}
                      {a.entity_type}
                    </span>
                  </Td>
                  <Td className="max-w-[26rem]">
                    <Details value={a.details} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        <Pager basePath="/audit" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}

function Details({ value }: { value: Record<string, unknown> }) {
  const entries = Object.entries(value ?? {});
  if (entries.length === 0) return <span className="text-faint">—</span>;
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-[0.8125rem]">
      {entries.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted">{k.replace(/_/g, " ")}</dt>
          <dd className="min-w-0 break-words text-ink-2">{typeof v === "string" ? v : JSON.stringify(v)}</dd>
        </div>
      ))}
    </dl>
  );
}

async function ChainStatus() {
  const i = await load((c) => c.GET("/integrity"));
  return i.audit_chain_ok ? (
    <p className="mb-5 text-sm text-ok">Chain verified intact just now.</p>
  ) : (
    <p role="alert" className="mb-5 rounded-md border border-bad/30 bg-bad-soft px-3 py-2.5 text-sm text-bad">
      The chain breaks at entry {i.first_bad_audit_id}: the log has been altered. Escalate this immediately.
    </p>
  );
}
