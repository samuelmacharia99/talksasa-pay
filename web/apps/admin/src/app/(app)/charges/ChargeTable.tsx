import type { AdminSchemas } from "@talksasa/api-client/admin";
import { Amount, DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import Link from "next/link";

type Charge = AdminSchemas["Charge"];

export function ChargeTable({ items, empty }: { items: Charge[]; empty: { title: string; body: string } }) {
  if (items.length === 0) return <Empty title={empty.title}>{empty.body}</Empty>;
  return (
    <Table label="Charges">
      <thead>
        <tr>
          <Th>Reference</Th>
          <Th>App</Th>
          <Th>Account number</Th>
          <Th>Created</Th>
          <Th>Status</Th>
          <Th align="right">Amount</Th>
        </tr>
      </thead>
      <tbody>
        {items.map((c) => (
          <tr key={c.id} className="hover:bg-wash/60">
            <Td>
              <Link href={`/charges/${c.id}`} className="font-medium text-ink hover:text-violet hover:underline">
                {c.reference}
              </Link>
              {c.description ? <span className="block max-w-[22rem] truncate text-[0.8125rem] text-muted">{c.description}</span> : null}
            </Td>
            <Td className="text-ink-2">{c.app_name}</Td>
            <Td className="font-mono text-[0.8125rem]">{c.account_number}</Td>
            <Td>
              <DateTime value={c.created_at} />
            </Td>
            <Td>
              <Status value={c.status} />
            </Td>
            <Td align="right">
              <Amount value={c.amount} bare className={c.status === "succeeded" ? "font-medium" : undefined} />
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
