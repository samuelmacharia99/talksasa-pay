import type { AdminSchemas } from "@talksasa/api-client/admin";
import { Amount, DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import Link from "next/link";
import type { ReactNode } from "react";

import { CHANNEL, KIND } from "@/lib/labels";

type Txn = AdminSchemas["Transaction"];

/**
 * Transactions, newest first. `action` adds a last column (e.g. "Assign to
 * app" on the unmatched list).
 */
export function TxnTable({ items, empty, action }: { items: Txn[]; empty: { title: string; body: string }; action?: (t: Txn) => ReactNode }) {
  if (items.length === 0) return <Empty title={empty.title}>{empty.body}</Empty>;
  return (
    <Table label="Transactions">
      <thead>
        <tr>
          <Th>When</Th>
          <Th>Receipt</Th>
          <Th>App</Th>
          <Th>Account number</Th>
          <Th>Charge</Th>
          <Th>How</Th>
          <Th>Status</Th>
          <Th align="right">Amount</Th>
          {action ? (
            <Th align="right">
              <span className="sr-only">Actions</span>
            </Th>
          ) : null}
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
              {t.app_id ? (
                <Link href={`/apps/${t.app_id}`} className="hover:underline">
                  {t.account_prefix}
                </Link>
              ) : (
                <span className="text-warn">Unmatched</span>
              )}
            </Td>
            <Td className="font-mono text-[0.8125rem]">{t.account_number ?? <span className="text-faint">—</span>}</Td>
            <Td>
              {t.charge_id ? (
                <Link href={`/charges/${t.charge_id}`} className="text-violet hover:underline">
                  {t.charge_reference ?? "View"}
                </Link>
              ) : (
                <span className="text-faint">—</span>
              )}
            </Td>
            <Td className="text-ink-2">{t.kind === "payment" ? (CHANNEL[t.channel] ?? t.channel) : (KIND[t.kind] ?? t.kind)}</Td>
            <Td>
              <Status value={t.status} />
            </Td>
            <Td align="right">
              <Amount value={t.direction === "out" ? -t.amount : t.amount} className={t.direction === "out" ? "text-bad" : "font-medium"} />
            </Td>
            {action ? <Td align="right">{action(t)}</Td> : null}
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
