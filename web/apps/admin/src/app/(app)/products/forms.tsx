"use client";

import { Button, cx, Field, SecretOnce } from "@talksasa/ui";
import Link from "next/link";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import type { ActionResult } from "@/lib/api";
import { MAX_ALLOWED_IPS, validateAllowedIPs } from "@/lib/webhooks";

import {
  type AllowedIPsResult,
  createProduct,
  issueAPIKey,
  revokeAPIKey,
  rotateWebhookSecret,
  type SecretResult,
  setAllowedIPs,
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
        help="For the next 24 hours every webhook carries two signatures, one per secret. Update the product's configuration with this one before then."
        value={state.data.secret}
      />
    );
  }
  return (
    <form action={action} className="grid gap-3">
      <FormError message={err(state)} />
      <p className="text-sm text-ink-2">
        Replace the secret the product uses to verify our webhooks. We keep signing with the old one as well for 24 hours,
        so the product can switch over without rejecting events.
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

/**
 * A key's IP allow-list: shown to everyone, editable by admins. The light
 * check here catches typos; the Go service is authoritative.
 */
export function AllowedIPs({ id, keyID, current, canEdit }: { id: string; keyID: string; current: string[] | null; canEdit: boolean }) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const [state, action] = useActionState(async (prev: AllowedIPsResult, fd: FormData) => {
    const r = await setAllowedIPs(id, keyID, prev, fd);
    if (r?.ok) setEditing(false);
    return r;
  }, undefined as AllowedIPsResult);
  const list = current && current.length > 0 ? current : null;

  if (!editing) {
    return (
      <div className="flex items-start gap-3">
        {list ? (
          <ul className="grid gap-0.5 font-mono text-[0.8125rem] text-ink-2">
            {list.map((ip) => (
              <li key={ip}>{ip}</li>
            ))}
          </ul>
        ) : (
          <span className="text-muted">Any address</span>
        )}
        {canEdit ? (
          <Button
            variant="ghost"
            size="sm"
            className="-my-1"
            onClick={() => {
              setText((list ?? []).join("\n"));
              setEditing(true);
            }}
          >
            Edit<span className="sr-only"> allowed IPs for key {keyID}</span>
          </Button>
        ) : null}
      </div>
    );
  }

  const problems = validateAllowedIPs(text);
  const failed = state && !state.ok ? state : undefined;
  const fieldErrors = (failed?.fieldErrors ?? []).filter((f) => f.field.startsWith("allowed_ips"));
  const inputId = `allowed-ips-${keyID}`;
  const invalid = problems.length > 0 || fieldErrors.length > 0;
  return (
    <form action={action} className="grid w-72 gap-2">
      {failed && fieldErrors.length === 0 ? <FormError message={failed.error} /> : null}
      <label htmlFor={inputId} className="text-[0.8125rem] font-medium text-ink-2">
        Allowed IPs, one per line
      </label>
      <textarea
        id={inputId}
        name="allowed_ips"
        rows={4}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={"203.0.113.4\n198.51.100.0/24"}
        aria-invalid={invalid || undefined}
        aria-describedby={`${inputId}-help`}
        className={cx(
          "block w-full rounded-md border border-line-strong bg-surface px-3 py-2 font-mono text-[0.8125rem] text-ink placeholder:text-faint",
          "focus:border-violet focus:outline-2 focus:outline-violet/25 aria-[invalid=true]:border-bad",
        )}
      />
      <div id={`${inputId}-help`} className="text-[0.8125rem]">
        {invalid ? (
          <ul role="alert" className="grid gap-0.5 text-bad">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
            {fieldErrors.map((f) => (
              <li key={f.field + f.message}>{f.message}</li>
            ))}
          </ul>
        ) : (
          <p className="text-muted">
            Addresses or CIDR ranges, up to {MAX_ALLOWED_IPS}. Leave empty to allow any address. Applies from the key&apos;s next request.
          </p>
        )}
      </div>
      <div className="flex flex-wrap gap-2">
        <SubmitButton variant="secondary" pending="Saving…" disabled={problems.length > 0} className="h-8 px-3 text-[0.8125rem]">
          Save
        </SubmitButton>
        <Button variant="ghost" size="sm" onClick={() => setEditing(false)}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
