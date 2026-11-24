import type { AdminSchemas } from "@talksasa/api-client/admin";
import { Amount, DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import Link from "next/link";

export function RequestTable({ items, showPayer = true }: { items: Array<AdminSchemas["PaymentRequest"]>; showPayer?: boolean }) {
  if (items.length === 0) return <Empty title="No bills match">Products create bills through the API; they appear here as soon as they do.</Empty>;
  return (
    <Table label="Payment requests">
      <thead>
        <tr>
          <Th>Bill</Th>
          {showPayer ? <Th>Payer</Th> : null}
          <Th>Created</Th>
          <Th>Status</Th>
          <Th align="right">Amount</Th>
          <Th align="right">Still to pay</Th>
        </tr>
      </thead>
      <tbody>
        {items.map((r) => (
          <tr key={r.id} className="hover:bg-wash/60">
            <Td>
              <Link href={`/requests/${r.id}`} className="font-medium text-ink hover:text-violet hover:underline">
                {r.product_reference}
              </Link>
              <span className="block max-w-[22rem] truncate text-[0.8125rem] text-muted">{r.description}</span>
              {r.duplicate_reference && (r.status === "open" || r.status === "partially_paid") ? (
                <span className="block text-[0.8125rem] text-warn">Another live bill has this reference</span>
              ) : null}
            </Td>
            {showPayer ? (
              <Td>
                <Link href={`/payers/${r.payer_account_id}`} className="hover:underline">
                  {r.payer_name}
                </Link>
                <span className="block text-[0.8125rem] text-muted">{r.account_number}</span>
              </Td>
            ) : null}
            <Td>
              <DateTime value={r.created_at} dateOnly />
            </Td>
            <Td>
              <Status value={r.status} />
            </Td>
            <Td align="right">
              <Amount value={r.amount} bare />
            </Td>
            <Td align="right">
              {r.outstanding > 0 && (r.status === "open" || r.status === "partially_paid") ? (
                <Amount value={r.outstanding} bare className="font-medium" />
              ) : (
                <span className="text-faint">—</span>
              )}
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
