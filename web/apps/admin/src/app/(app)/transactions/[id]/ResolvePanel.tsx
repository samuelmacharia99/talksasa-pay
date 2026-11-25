"use client";

import { Button, cx, Field, inputClass } from "@talksasa/ui";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { normalizeReceipt, type ResolveAction, validateResolve } from "@/lib/mpesa";

import { resolveTransaction, type ResolveResult } from "./actions";

/**
 * Resolve an STK payment in needs_review: Safaricom said it was paid but
 * sent no receipt. Staff look it up on the M-Pesa portal, then either
 * confirm it with the receipt or mark it not paid. Two steps: fill in, then
 * confirm what will happen.
 */
export function ResolvePanel({ id, amountLabel, phone }: { id: string; amountLabel: string; phone: string | null }) {
  const [action, setAction] = useState<ResolveAction | null>(null);
  return (
    <div className="grid gap-4">
      <p className="text-sm text-ink-2">
        Safaricom said this prompt was paid but sent no receipt. Find the payment on the M-Pesa portal
        {phone ? (
          <>
            {" "}
            (from <span className="font-mono">{phone}</span>, {amountLabel})
          </>
        ) : (
          <> ({amountLabel})</>
        )}
        . If it is there, confirm it with its receipt: the charge then succeeds and the app is told. If it is not, mark it as not paid.
      </p>
      {action === null ? (
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => setAction("confirm")}>Confirm with receipt</Button>
          <Button variant="danger" onClick={() => setAction("fail")}>
            Mark as not paid
          </Button>
        </div>
      ) : (
        <ResolveForm key={action} id={id} action={action} amountLabel={amountLabel} onCancel={() => setAction(null)} />
      )}
    </div>
  );
}

function ResolveForm({ id, action, amountLabel, onCancel }: { id: string; action: ResolveAction; amountLabel: string; onCancel: () => void }) {
  const [state, submit] = useActionState(resolveTransaction.bind(null, id, action), undefined as ResolveResult);
  const [receipt, setReceipt] = useState("");
  const [reason, setReason] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [local, setLocal] = useState<Partial<Record<string, string>>>({});

  if (state?.ok) {
    return (
      <p role="status" className="text-sm text-ok">
        {action === "confirm" ? "Confirmed. The payment is recorded and the app is told." : "Marked as not paid."}
      </p>
    );
  }

  const server = state && !state.ok ? state : undefined;
  const fieldError = (f: string) => local[f] ?? server?.fieldErrors?.find((e) => e.field === f)?.message;
  const confirm = action === "confirm";

  return (
    <form action={submit} className="grid max-w-xl gap-4">
      <h3 className="font-display text-[0.95rem] font-semibold">{confirm ? "Confirm with receipt" : "Mark as not paid"}</h3>
      {server && !server.fieldErrors?.length ? <FormError message={server.error} /> : null}

      {confirm ? (
        <Field
          label="M-Pesa receipt"
          name="receipt"
          value={receipt}
          onChange={(e) => setReceipt(e.target.value)}
          readOnly={confirming}
          autoComplete="off"
          spellCheck={false}
          maxLength={64}
          placeholder="SKL4ABCD12"
          className="[&_input]:font-mono [&_input]:uppercase"
          hint="Exactly as shown on the M-Pesa portal."
          error={fieldError("receipt")}
        />
      ) : null}

      <div className="grid gap-1.5">
        <label htmlFor="resolve-reason" className="text-[0.8125rem] font-medium text-ink-2">
          Reason
        </label>
        <textarea
          id="resolve-reason"
          name="reason"
          rows={3}
          maxLength={500}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          readOnly={confirming}
          aria-invalid={fieldError("reason") ? true : undefined}
          aria-describedby={fieldError("reason") ? "resolve-reason-error" : "resolve-reason-hint"}
          className={cx(inputClass, "h-auto py-2")}
          placeholder={confirm ? "Found on the M-Pesa portal, 24 Nov 10:42" : "Not on the M-Pesa portal after 30 minutes"}
        />
        {fieldError("reason") ? (
          <p id="resolve-reason-error" role="alert" className="text-[0.8125rem] text-bad">
            {fieldError("reason")}
          </p>
        ) : (
          <p id="resolve-reason-hint" className="text-[0.8125rem] text-muted">
            Kept in the audit log with your name.
          </p>
        )}
      </div>

      {confirming ? (
        <div className={cx("grid gap-3 rounded-md border px-4 py-3", confirm ? "border-violet/30 bg-violet-soft" : "border-bad/30 bg-bad-soft")}>
          <p className="text-sm text-ink">
            {confirm ? (
              <>
                Record receipt <span className="font-mono font-medium">{normalizeReceipt(receipt)}</span> for {amountLabel}? The charge
                succeeds and the app is told. This cannot be undone here.
              </>
            ) : (
              <>Mark this {amountLabel} prompt as not paid? The charge goes back to pending and the customer can pay again.</>
            )}
          </p>
          <div className="flex flex-wrap gap-2">
            <SubmitButton variant={confirm ? "primary" : "danger"} pending="Saving…">
              {confirm ? "Yes, confirm payment" : "Yes, mark as not paid"}
            </SubmitButton>
            <Button variant="ghost" onClick={() => setConfirming(false)}>
              Back
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button
            variant={confirm ? "primary" : "danger"}
            onClick={() => {
              const errors = validateResolve({ action, receipt, reason });
              setLocal(errors);
              if (Object.keys(errors).length === 0) setConfirming(true);
            }}
          >
            Continue
          </Button>
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      )}
    </form>
  );
}
