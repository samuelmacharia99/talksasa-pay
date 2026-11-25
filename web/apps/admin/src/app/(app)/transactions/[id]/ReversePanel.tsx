"use client";

import { Button, cx, Field, inputClass } from "@talksasa/ui";
import Link from "next/link";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { validateReverse } from "@/lib/mpesa";

import { reverseTransaction, type ReverseResult } from "./actions";

/**
 * Records a reversal of a succeeded payment (admin only, money rule 9): a
 * reversal transaction for the full amount; a charge it paid becomes
 * reversed and the app gets transaction.reversed. Staff give a reason and
 * Safaricom's reversal receipt (M-Pesa), then type the payment's receipt to
 * confirm.
 */
export function ReversePanel({
  id,
  amountLabel,
  receiptRequired,
  expected,
  chargeReference,
}: {
  id: string;
  amountLabel: string;
  receiptRequired: boolean;
  expected: string;
  chargeReference: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [state, submit] = useActionState(reverseTransaction.bind(null, id, { receiptRequired, expected }), undefined as ReverseResult);
  const [reason, setReason] = useState("");
  const [receipt, setReceipt] = useState("");
  const [confirm, setConfirm] = useState("");
  const [local, setLocal] = useState<Partial<Record<string, string>>>({});

  if (state?.ok) {
    return (
      <p role="status" className="text-sm text-ok">
        Reversed. The app has been told.{" "}
        <Link href={`/transactions/${state.data.reversalId}`} className="font-medium text-violet hover:underline">
          View the reversal
        </Link>
      </p>
    );
  }

  const intro = (
    <p className="text-sm text-ink-2">
      Record that this {amountLabel} payment was reversed, for example after Safaricom reversed it at the customer’s request. This adds a
      reversal for the full amount{chargeReference ? <>, marks charge {chargeReference} as reversed</> : null} and tells the app. It does
      not move any money itself.
    </p>
  );

  if (!open) {
    return (
      <div className="grid gap-4">
        {intro}
        <div>
          <Button variant="danger" onClick={() => setOpen(true)}>
            Reverse payment
          </Button>
        </div>
      </div>
    );
  }

  const server = state && !state.ok ? state : undefined;
  const fieldError = (f: string) => local[f] ?? server?.fieldErrors?.find((e) => e.field === f)?.message;
  const check = () => validateReverse({ reason, receipt, confirm, receiptRequired, expected });

  return (
    <form
      action={submit}
      onSubmit={(e) => {
        const errors = check();
        setLocal(errors);
        if (Object.keys(errors).length > 0) e.preventDefault();
      }}
      className="grid max-w-xl gap-4"
    >
      {intro}
      {server && !server.fieldErrors?.length ? <FormError message={server.error} /> : null}

      <div className="grid gap-1.5">
        <label htmlFor="reverse-reason" className="text-[0.8125rem] font-medium text-ink-2">
          Reason
        </label>
        <textarea
          id="reverse-reason"
          name="reason"
          rows={3}
          maxLength={500}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          aria-invalid={fieldError("reason") ? true : undefined}
          aria-describedby={fieldError("reason") ? "reverse-reason-error" : "reverse-reason-hint"}
          className={cx(inputClass, "h-auto py-2")}
          placeholder="Safaricom reversed it at the customer’s request, ticket 4411"
        />
        {fieldError("reason") ? (
          <p id="reverse-reason-error" role="alert" className="text-[0.8125rem] text-bad">
            {fieldError("reason")}
          </p>
        ) : (
          <p id="reverse-reason-hint" className="text-[0.8125rem] text-muted">
            Kept in the audit log with your name.
          </p>
        )}
      </div>

      <Field
        label={receiptRequired ? "Reversal receipt" : "Reversal receipt (optional)"}
        name="receipt"
        value={receipt}
        onChange={(e) => setReceipt(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        maxLength={64}
        placeholder="SRV1ABCD23"
        className="[&_input]:font-mono [&_input]:uppercase"
        hint="Safaricom’s receipt for the reversal, from the M-Pesa portal."
        error={fieldError("receipt")}
      />

      <Field
        label={`Type ${expected} to confirm`}
        name="confirm"
        value={confirm}
        onChange={(e) => setConfirm(e.target.value)}
        autoComplete="off"
        spellCheck={false}
        className="[&_input]:font-mono [&_input]:uppercase"
        error={fieldError("confirm")}
      />

      <div className="flex flex-wrap gap-2">
        <SubmitButton variant="danger" pending="Reversing…">
          Reverse {amountLabel}
        </SubmitButton>
        <Button variant="ghost" onClick={() => setOpen(false)}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
