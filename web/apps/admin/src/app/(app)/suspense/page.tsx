import { Amount } from "@talksasa/ui";
import type { Metadata } from "next";

import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { load } from "@/lib/api";
import { pick } from "@/lib/params";

import { TxnTable } from "../transactions/TxnTable";

export const metadata: Metadata = { title: "Unmatched payments" };

export default async function Suspense({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, ["cursor"] as const);
  const [page, dash] = await Promise.all([
    load((c) => c.GET("/transactions", { params: { query: { suspense: "true", limit: 50, cursor: p.cursor } } })),
    load((c) => c.GET("/dashboard")),
  ]);
  return (
    <>
      <PageHeader
        title="Unmatched payments"
        lead={
          <>
            Money received with an account number that matches no payer, usually a typo. It is recorded and safe:{" "}
            <Amount value={dash.counts.suspense_amount} /> across {dash.counts.suspense_count}{" "}
            {dash.counts.suspense_count === 1 ? "payment" : "payments"}. Assigning a payment to the right payer arrives with the
            reconciliation tools.
          </>
        }
      />
      <Panel flush>
        <TxnTable items={page.items} empty={{ title: "Nothing unmatched", body: "Every payment has been matched to a payer." }} />
        <Pager basePath="/suspense" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
