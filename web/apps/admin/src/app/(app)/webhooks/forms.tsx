"use client";

import { Button } from "@talksasa/ui";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";

import { redeliver, type RedeliverResult } from "./actions";

export function RedeliverForm({ eventID, productLabel }: { eventID: string; productLabel: string }) {
  const [state, action] = useActionState(redeliver.bind(null, eventID), undefined as RedeliverResult);
  const [confirming, setConfirming] = useState(false);
  if (state?.ok) {
    return (
      <p role="status" className="text-sm text-ok">
        Queued again. The first new attempt runs shortly; reload this page to see it.
      </p>
    );
  }
  return (
    <form action={action} className="grid gap-3">
      <FormError message={state && !state.ok ? state.error : undefined} />
      <p className="text-sm text-ink-2">
        Send this event to {productLabel} again, with fresh retries. Do this once the product has fixed whatever made it
        fail. The product must already ignore events it has seen, so a repeat is harmless.
      </p>
      {confirming ? (
        <div className="flex flex-wrap gap-2">
          <SubmitButton pending="Queueing…">Yes, send it again</SubmitButton>
          <Button variant="ghost" onClick={() => setConfirming(false)}>
            Cancel
          </Button>
        </div>
      ) : (
        <div>
          <Button variant="secondary" onClick={() => setConfirming(true)}>
            Redeliver
          </Button>
        </div>
      )}
    </form>
  );
}
