import type { AdminSchemas } from "@talksasa/api-client/admin";
import { Amount, DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import Link from "next/link";

import { CHANNEL, KIND } from "@/lib/labels";

type Txn = AdminSchemas["Transaction"];

export function TxnTable({ items, empty }: { items: Txn[]; empty: { title: string; body: string } }) {
  if (items.length === 0) return <Empty title={empty.title}>{empty.body}</Empty>;
  return (
    <Table label="Transactions">
      <thead>
        <tr>
          <Th>When</Th>
          <Th>Receipt</Th>
          <Th>Account</Th>
          <Th>How</Th>
          <Th>Status</Th>
          <Th align="right">Amount</Th>
        </tr>
      </thead>
      <tbody>
        {items.map((t) => (
          <tr key={t.id} className="hover:bg-wash/60">
            <Td>
              <Link href={`/transactions/${t.id}`} className="text-ink hover:text-violet hover:underline">
                <DateTime value={t.created_at} />
              </Link>
            </Td>
            <Td className="font-mono text-[0.8125rem]">{t.receipt ?? <span className="text-faint">none yet</span>}</Td>
            <Td>
              {t.account_number ? (
                <Link href={`/payers/${t.payer_account_id}`} className="hover:underline">
                  {t.account_number}
                </Link>
              ) : (
                <span className="text-warn">{t.bill_ref ? `typed “${t.bill_ref}”` : "no account"}</span>
              )}
            </Td>
            <Td className="text-ink-2">
              {t.kind === "payment" ? CHANNEL[t.channel] ?? t.channel : KIND[t.kind] ?? t.kind}
            </Td>
            <Td>
              <Status value={t.status} />
            </Td>
            <Td align="right">
              <Amount value={t.direction === "out" ? -t.amount : t.amount} className={t.direction === "out" ? "text-bad" : "font-medium"} />
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
