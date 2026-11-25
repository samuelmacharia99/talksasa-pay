"use client";

import { Field, formatDateTime, SelectField } from "@talksasa/ui";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { ToneLabel } from "@/components/ToneLabel";
import type { ActionResult } from "@/lib/api";
import { checkSettingsForm, CONFIRM_WORD, type Credential, credentialLabel, maskCallbackURL, needsConfirmation } from "@/lib/settings";

import { registerURLs, saveSettings, testCredentials } from "./actions";

function fieldError(s: ActionResult<unknown> | undefined, field: string): string | undefined {
  if (!s || s.ok) return undefined;
  return s.fieldErrors?.find((e) => e.field === field)?.message ?? (s.field === field ? s.error : undefined);
}

type PaybillValues = {
  environment: string | null;
  shortcode: string | null;
  stk_shortcode: string | null;
  callback_base_url: string | null;
  credentials: { consumer_key: Credential; consumer_secret: Credential; passkey: Credential };
};

export function PaybillForm({
  current,
  savedEnvironment,
  environments,
  httpsOnly,
}: {
  current: PaybillValues;
  savedEnvironment: string | null;
  environments: string[];
  httpsOnly: boolean;
}) {
  const [state, action] = useActionState(saveSettings, undefined);
  const [env, setEnv] = useState(current.environment ?? environments[0] ?? "sandbox");
  const [clientError, setClientError] = useState<{ field: string; message: string } | undefined>();
  const confirm = needsConfirmation(savedEnvironment, env);
  const err = (f: string) => (clientError?.field === f ? clientError.message : fieldError(state, f));

  return (
    <form
      action={action}
      onSubmit={(e) => {
        const fd = new FormData(e.currentTarget);
        const problem = checkSettingsForm({
          shortcode: String(fd.get("shortcode") ?? "").trim(),
          stk_shortcode: String(fd.get("stk_shortcode") ?? "").trim(),
          callback_base_url: String(fd.get("callback_base_url") ?? "").trim(),
          https_only: httpsOnly,
        });
        setClientError(problem);
        if (problem) e.preventDefault();
      }}
      className="grid gap-4"
      autoComplete="off"
    >
      <FormError message={state && !state.ok && !state.field ? state.error : undefined} />
      {state?.ok ? <p className="text-sm text-ok">Settings saved. The api and worker switch to them within a few seconds.</p> : null}
      <div className="grid gap-4 sm:grid-cols-2">
        <SelectField label="Environment" name="environment" value={env} onChange={(e) => setEnv(e.target.value)} error={err("environment")}>
          {environments.map((v) => (
            <option key={v} value={v}>
              {v === "production" ? "Production" : "Sandbox"}
            </option>
          ))}
        </SelectField>
        <Field
          label="Paybill number"
          name="shortcode"
          required
          inputMode="numeric"
          pattern="[0-9]{5,7}"
          defaultValue={current.shortcode ?? ""}
          error={err("shortcode")}
        />
        <Field
          label="STK shortcode (optional)"
          name="stk_shortcode"
          inputMode="numeric"
          pattern="[0-9]{5,7}"
          defaultValue={current.stk_shortcode ?? ""}
          hint="Leave empty to send prompts from the paybill."
          error={err("stk_shortcode")}
        />
        <Field
          label="Callback base URL"
          name="callback_base_url"
          required
          type="url"
          placeholder="https://api.pay.example.com/"
          defaultValue={current.callback_base_url ?? ""}
          hint="The public address Safaricom calls back. Ends with /."
          error={err("callback_base_url")}
        />
      </div>
      <fieldset className="grid gap-4 sm:grid-cols-3">
        <legend className="mb-2 text-sm text-muted">Daraja credentials. They are stored encrypted and never shown again. Leave a field empty to keep the saved value.</legend>
        {(
          [
            ["consumer_key", "Consumer key"],
            ["consumer_secret", "Consumer secret"],
            ["passkey", "Passkey"],
          ] as const
        ).map(([name, label]) => (
          <Field
            key={name}
            label={label}
            name={name}
            type="password"
            autoComplete="new-password"
            spellCheck={false}
            hint={credentialLabel(current.credentials[name])}
            required={!current.credentials[name].set}
            error={err(name)}
          />
        ))}
      </fieldset>
      {confirm ? (
        <Field
          label={`Type "${CONFIRM_WORD}" to confirm`}
          name="confirm"
          required
          pattern={CONFIRM_WORD}
          autoComplete="off"
          hint="These settings move real money."
          error={err("confirm")}
          className="max-w-xs"
        />
      ) : null}
      <div>
        <SubmitButton pending="Saving…">Save settings</SubmitButton>
      </div>
    </form>
  );
}

export function TestForm({ last }: { last: { ok: boolean; tested_at: string } | null }) {
  const [state, action] = useActionState(testCredentials, undefined);
  const shown = state?.ok ? state.data : last ? { ...last, message: null } : null;
  return (
    <form action={action} className="grid gap-3">
      <FormError message={state && !state.ok ? state.error : undefined} />
      <p className="text-sm text-ink-2">Asks Safaricom for an access token with the saved consumer key and secret. The token is not kept or shown.</p>
      {shown ? (
        <p className="flex flex-wrap items-center gap-2 text-sm" data-testid="test-result">
          <ToneLabel label={shown.ok ? "Credentials accepted" : "Credentials refused"} tone={shown.ok ? "ok" : "bad"} />
          <span className="text-muted">{formatDateTime(shown.tested_at)}</span>
          {shown.message ? <span className="w-full text-bad">{shown.message}</span> : null}
        </p>
      ) : null}
      <div>
        <SubmitButton variant="secondary" pending="Testing…">
          Test credentials
        </SubmitButton>
      </div>
    </form>
  );
}

export function RegisterForm({ production, disabled }: { production: boolean; disabled: boolean }) {
  const [state, action] = useActionState(registerURLs, undefined);
  return (
    <form action={action} className="grid gap-3">
      <FormError message={state && !state.ok && state.field !== "confirm" ? state.error : undefined} />
      {state?.ok ? (
        <p className="text-sm text-ok">
          Safaricom accepted {maskCallbackURL(state.data.confirmation_url)} at {formatDateTime(state.data.registered_at)}.
        </p>
      ) : null}
      {production ? (
        <Field
          label={`Type "${CONFIRM_WORD}" to confirm`}
          name="confirm"
          required
          pattern={CONFIRM_WORD}
          autoComplete="off"
          error={fieldError(state, "confirm")}
          className="max-w-xs"
        />
      ) : null}
      <div>
        <SubmitButton pending="Registering…" disabled={disabled}>
          Register URLs with Safaricom
        </SubmitButton>
      </div>
    </form>
  );
}
