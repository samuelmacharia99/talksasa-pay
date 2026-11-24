"use client";

import { Button, Field, SecretOnce } from "@talksasa/ui";
import Link from "next/link";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import type { ActionResult } from "@/lib/api";

import {
  createProduct,
  issueAPIKey,
  revokeAPIKey,
  rotateWebhookSecret,
  type SecretResult,
  setProductStatus,
  setWebhookURL,
} from "./actions";

const err = (s: ActionResult<unknown> | undefined) => (s && !s.ok ? s.error : undefined);

export function CreateProductForm() {
  const [state, action] = useActionState(createProduct, undefined);
  if (state?.ok) {
    return (
      <div className="grid gap-4">
        <SecretOnce
          title="Product created. Copy its webhook signing secret now."
          help="The product's developers need this to verify our webhooks. It is never shown again; rotate it if it is lost."
          value={state.data.secret}
        />
        <Link href={`/products/${state.data.productId}`} className="text-sm font-medium text-violet hover:underline">
          Open the product to issue an API key
        </Link>
      </div>
    );
  }
  return (
    <form action={action} className="grid gap-4 sm:grid-cols-[1fr_9rem] sm:items-start">
      <div className="sm:col-span-2">
        <FormError message={err(state)} />
      </div>
      <Field label="Product name" name="name" required maxLength={100} placeholder="Domains" />
      <Field
        label="Account prefix"
        name="account_prefix"
        required
        pattern="[A-Za-z]{2,6}"
        maxLength={6}
        placeholder="DOM"
        className="[&_input]:uppercase"
        hint="2–6 letters. Permanent."
      />
      <Field
        label="Webhook URL (optional)"
        name="webhook_url"
        type="url"
        placeholder="https://domains.talksasa.co.ke/hooks/pay"
        className="sm:col-span-2"
      />
      <div className="sm:col-span-2">
        <SubmitButton pending="Creating…">Create product</SubmitButton>
      </div>
    </form>
  );
}

export function WebhookForm({ id, current }: { id: string; current: string }) {
  const [state, action] = useActionState(setWebhookURL.bind(null, id), undefined);
  return (
    <form action={action} className="grid gap-3">
      <FormError message={err(state)} />
      {state?.ok ? <p className="text-sm text-ok">Webhook URL saved.</p> : null}
      <div className="flex flex-wrap items-end gap-3">
        <Field
          label="Webhook URL"
          name="webhook_url"
          type="url"
          defaultValue={current}
          placeholder="https://…"
          className="min-w-0 flex-1"
          hint="Leave empty to stop sending events."
        />
        <SubmitButton variant="secondary" pending="Saving…" className="mb-[1.6rem]">
          Save
        </SubmitButton>
      </div>
    </form>
  );
}

export function RotateSecretForm({ id }: { id: string }) {
  const [state, action] = useActionState(rotateWebhookSecret.bind(null, id), undefined as SecretResult);
  const [confirming, setConfirming] = useState(false);
  if (state?.ok) {
    return (
      <SecretOnce
        title="New webhook signing secret"
        help="The old secret stopped working just now. Update the product's configuration with this one."
        value={state.data.secret}
      />
    );
  }
  return (
    <form action={action} className="grid gap-3">
      <FormError message={err(state)} />
      <p className="text-sm text-ink-2">
        Replace the secret the product uses to verify our webhooks. The old one stops working immediately.
      </p>
      {confirming ? (
        <div className="flex flex-wrap gap-2">
          <SubmitButton variant="danger" pending="Rotating…">
            Yes, replace the secret
          </SubmitButton>
          <Button variant="ghost" onClick={() => setConfirming(false)}>
            Keep the current one
          </Button>
        </div>
      ) : (
        <div>
          <Button variant="secondary" onClick={() => setConfirming(true)}>
            Rotate secret
          </Button>
        </div>
      )}
    </form>
  );
}

export function StatusForm({ id, status }: { id: string; status: "active" | "suspended" }) {
  const next = status === "active" ? "suspended" : "active";
  const [state, action] = useActionState(setProductStatus.bind(null, id, next), undefined);
  const [confirming, setConfirming] = useState(false);
  return (
    <form action={action} className="grid gap-3">
      <FormError message={err(state)} />
      <p className="text-sm text-ink-2">
        {status === "active"
          ? "Suspending blocks the product's API keys and its payment links at once. Money customers still send is recorded as usual."
          : "Re-activating restores the product's API keys and payment links."}
      </p>
      {status === "active" && !confirming ? (
        <div>
          <Button variant="danger" onClick={() => setConfirming(true)}>
            Suspend product
          </Button>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2">
          <SubmitButton variant={status === "active" ? "danger" : "primary"} pending="Saving…">
            {status === "active" ? "Yes, suspend it" : "Re-activate product"}
          </SubmitButton>
          {confirming ? (
            <Button variant="ghost" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          ) : null}
        </div>
      )}
    </form>
  );
}

export function IssueKeyForm({ id }: { id: string }) {
  const [state, action] = useActionState(issueAPIKey.bind(null, id), undefined as SecretResult);
  if (state?.ok) {
    return (
      <SecretOnce
        title="API key issued"
        help="Put it in the product's secret manager now. We store only a fingerprint, so it can never be shown again."
        value={state.data.secret}
      />
    );
  }
  return (
    <form action={action} className="grid gap-3">
      <FormError message={err(state)} />
      <div className="flex flex-wrap items-end gap-3">
        <Field label="Label" name="label" required maxLength={100} placeholder="production, March 2027" className="min-w-0 flex-1" />
        <Field label="Expires (optional)" name="expires" type="date" />
        <SubmitButton pending="Issuing…" className="mb-px">
          Issue key
        </SubmitButton>
      </div>
    </form>
  );
}

export function RevokeKeyButton({ id, keyID }: { id: string; keyID: string }) {
  const [state, action] = useActionState(revokeAPIKey.bind(null, id, keyID), undefined);
  const [confirming, setConfirming] = useState(false);
  if (!confirming) {
    return (
      <Button variant="danger" size="sm" onClick={() => setConfirming(true)}>
        Revoke
      </Button>
    );
  }
  return (
    <form action={action} className="inline-flex items-center gap-2">
      {err(state) ? <span className="text-[0.8125rem] text-bad">{err(state)}</span> : null}
      <SubmitButton variant="danger" pending="Revoking…" className="h-8 px-3 text-[0.8125rem]">
        Revoke now
      </SubmitButton>
      <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
        Cancel
      </Button>
    </form>
  );
}
