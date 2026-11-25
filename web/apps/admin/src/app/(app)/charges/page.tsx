import type { Metadata } from "next";

import { AppSelect } from "@/components/AppSelect";
import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { load, type QueryOf } from "@/lib/api";
import { CHARGE_STATUS } from "@/lib/labels";
import { anySet, pick } from "@/lib/params";

import { ChargeTable } from "./ChargeTable";

export const metadata: Metadata = { title: "Charges" };

const KEYS = ["app_id", "status", "q", "from", "to", "cursor"] as const;

export default async function Charges({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const [page, apps] = await Promise.all([
    load((c) => c.GET("/charges", { params: { query: { ...p, limit: 50 } as QueryOf<"/charges"> } })),
    load((c) => c.GET("/apps")),
  ]);
  return (
    <>
      <PageHeader
        title="Charges"
        lead="Payments apps have asked customers to make, newest first. Each charge is one fixed amount for the app's own reference."
      />
      <FilterBar action="/charges" active={anySet(p)}>
        <FilterField label="Search">
          <input name="q" defaultValue={p.q} placeholder="Reference, description or account" className={`${filterInput} w-64`} />
        </FilterField>
        <FilterField label="App">
          <AppSelect apps={apps.items} value={p.app_id} />
        </FilterField>
        <FilterField label="Status">
          <select name="status" defaultValue={p.status ?? ""} className={filterInput}>
            <option value="">Any</option>
            {Object.entries(CHARGE_STATUS).map(([value, s]) => (
              <option key={value} value={value}>
                {s.label}
              </option>
            ))}
          </select>
        </FilterField>
        <FilterField label="From">
          <input type="date" name="from" defaultValue={p.from} className={filterInput} />
        </FilterField>
        <FilterField label="To">
          <input type="date" name="to" defaultValue={p.to} className={filterInput} />
        </FilterField>
      </FilterBar>
      <Panel flush>
        <ChargeTable
          items={page.items}
          empty={
            anySet(p)
              ? { title: "No charges match", body: "Try another status or a wider date range, or clear the filters." }
              : { title: "No charges yet", body: "Apps create charges through the API; they appear here as soon as they do." }
          }
        />
        <Pager basePath="/charges" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
