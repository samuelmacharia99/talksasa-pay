"use client";

import { Button, cx, inputClass } from "@talksasa/ui";
import { useActionState, useId, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { validateAssign } from "@/lib/mpesa";

import { assignTransaction, type AssignResult } from "./actions";

export interface AssignableApp {
  id: string;
  name: string;
  account_prefix: string;
  status: string;
}

/**
 * Assigns an unmatched payment (unknown account prefix) to an app, which
 * then gets payment.received. Finance and admin; audited with the reason.
 * `compact` starts as a single button, for table rows.
 */
export function AssignForm({
  id,
  apps,
  amountLabel,
  accountNumber,
  compact = false,
}: {
  id: string;
  apps: AssignableApp[];
  amountLabel: string;
  accountNumber: string | null;
  compact?: boolean;
}) {
  const uid = useId();
  const [open, setOpen] = useState(!compact);
  const [state, submit] = useActionState(assignTransaction.bind(null, id), undefined as AssignResult);
  const [appId, setAppId] = useState("");
  const [reason, setReason] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [local, setLocal] = useState<Partial<Record<string, string>>>({});

  if (state?.ok) {
    return (
      <p role="status" className="text-sm text-ok">
        Assigned to {state.data.appName ?? "the app"}. The app has been told.
        {state.data.appStatus && state.data.appStatus !== "active" ? (
          <span className="block text-warn">That app is suspended: tell its team the money arrived.</span>
        ) : null}
      </p>
    );
  }

  if (!open) {
    return (
      <Button variant="secondary" size="sm" onClick={() => setOpen(true)}>
        Assign to app<span className="sr-only"> ({amountLabel})</span>
      </Button>
    );
  }

  const server = state && !state.ok ? state : undefined;
  const fieldError = (f: string) => local[f] ?? server?.fieldErrors?.find((e) => e.field === f)?.message;
  const chosen = apps.find((a) => a.id === appId);

  return (
    <form action={submit} className={cx("grid gap-3 text-left", compact ? "w-72" : "max-w-xl")}>
      {server && !server.fieldErrors?.length ? <FormError message={server.error} /> : null}
      <div className="grid gap-1.5">
        <label htmlFor={`${uid}-app`} className="text-[0.8125rem] font-medium text-ink-2">
          App
        </label>
        <select
          id={`${uid}-app`}
          name="app_id"
          value={appId}
          onChange={(e) => {
            setAppId(e.target.value);
            setConfirming(false);
          }}
          disabled={confirming}
          aria-invalid={fieldError("app_id") ? true : undefined}
          className={inputClass}
        >
          <option value="">Choose an app</option>
          {apps.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name} ({a.account_prefix}){a.status !== "active" ? ", suspended" : ""}
            </option>
          ))}
        </select>
        {/* A disabled select is not submitted; keep the value in the form while confirming. */}
        {confirming ? <input type="hidden" name="app_id" value={appId} /> : null}
        {fieldError("app_id") ? (
          <p role="alert" className="text-[0.8125rem] text-bad">
            {fieldError("app_id")}
          </p>
        ) : null}
      </div>
      <div className="grid gap-1.5">
        <label htmlFor={`${uid}-reason`} className="text-[0.8125rem] font-medium text-ink-2">
          Reason
        </label>
        <textarea
          id={`${uid}-reason`}
          name="reason"
          rows={2}
          maxLength={500}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          readOnly={confirming}
          aria-invalid={fieldError("reason") ? true : undefined}
          className={cx(inputClass, "h-auto py-2")}
          placeholder="Customer typed HSOT for HOST; confirmed by phone"
        />
        {fieldError("reason") ? (
          <p role="alert" className="text-[0.8125rem] text-bad">
            {fieldError("reason")}
          </p>
        ) : (
          <p className="text-[0.8125rem] text-muted">Kept in the audit log with your name.</p>
        )}
      </div>
      {confirming && chosen ? (
        <div className="grid gap-3 rounded-md border border-violet/30 bg-violet-soft px-4 py-3">
          <p className="text-sm text-ink">
            Send this {amountLabel} payment{accountNumber ? <> for “{accountNumber}”</> : null} to {chosen.name}? The app is told at once
            and this cannot be undone here.
            {chosen.status !== "active" ? <span className="mt-1 block text-warn">{chosen.name} is suspended; the money is still forwarded.</span> : null}
          </p>
          <div className="flex flex-wrap gap-2">
            <SubmitButton pending="Assigning…">Yes, assign it</SubmitButton>
            <Button variant="ghost" onClick={() => setConfirming(false)}>
              Back
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button
            onClick={() => {
              const errors = validateAssign({ appId, reason });
              setLocal(errors);
              if (Object.keys(errors).length === 0) setConfirming(true);
            }}
          >
            Continue
          </Button>
          {compact ? (
            <Button variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
          ) : null}
        </div>
      )}
    </form>
  );
}
