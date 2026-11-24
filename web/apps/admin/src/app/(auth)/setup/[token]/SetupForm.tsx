"use client";

import { buttonClass, CopyButton, Field } from "@talksasa/ui";
import Link from "next/link";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";

import type { FormState } from "../../actions";

export function SetupForm({
  action,
  qrDataUrl,
  secret,
  enrolment,
}: {
  action: (state: FormState, fd: FormData) => Promise<FormState>;
  qrDataUrl: string;
  secret: string;
  enrolment: string;
}) {
  const [state, formAction] = useActionState(action, undefined);
  // Controlled, so a failed attempt (wrong code, weak password) doesn't wipe
  // what the person typed: React resets uncontrolled fields after an action.
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [code, setCode] = useState("");
  const mismatch = confirm.length > 0 && confirm !== password;

  if (state?.recoveryCodes) {
    const all = state.recoveryCodes.join("\n");
    return (
      <div className="mt-8">
        <h2 className="font-display text-lg font-semibold">Save your recovery codes</h2>
        <p className="mt-2 text-sm text-ink-2">
          If you lose your phone, each code signs you in once. They are shown only now: store them in your password manager.
        </p>
        <ol className="mt-5 grid grid-cols-2 gap-x-6 gap-y-2 rounded-lg border border-line bg-surface p-5 font-mono text-[0.9375rem]">
          {state.recoveryCodes.map((c) => (
            <li key={c} className="num">
              {c}
            </li>
          ))}
        </ol>
        <div className="mt-4 flex items-center gap-3">
          <CopyButton value={all} label="Copy all codes" />
        </div>
        <Link href="/login" className={buttonClass("primary", "md", "mt-8 w-full")}>
          Continue to sign in
        </Link>
      </div>
    );
  }

  return (
    <form
      action={formAction}
      onSubmit={(e) => {
        if (password !== confirm) e.preventDefault();
      }}
      className="mt-8 grid gap-6"
    >
      <FormError message={state?.error} />
      <input type="hidden" name="enrolment" value={enrolment} />
      <fieldset className="grid gap-5">
        <legend className="mb-1 font-medium text-ink">Choose a password</legend>
        <Field
          label="Password"
          name="password"
          type="password"
          autoComplete="new-password"
          minLength={12}
          maxLength={128}
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          hint="At least 12 characters. A few unrelated words works well."
        />
        <Field
          label="Repeat password"
          name="confirm"
          type="password"
          autoComplete="new-password"
          minLength={12}
          required
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          error={mismatch ? "The two passwords don’t match." : undefined}
        />
      </fieldset>
      <fieldset className="grid gap-4">
        <legend className="mb-1 font-medium text-ink">Connect your authenticator app</legend>
        <p className="text-sm text-ink-2">
          Scan this with Google Authenticator, Microsoft Authenticator, 1Password or similar.
        </p>
        <div className="flex flex-wrap items-center gap-5">
          {/* eslint-disable-next-line @next/next/no-img-element -- data URL generated on the server */}
          <img src={qrDataUrl} alt="QR code for your authenticator app" width={160} height={160} className="rounded-md border border-line" />
          <div className="min-w-0 flex-1 text-sm">
            <p className="text-muted">Can’t scan? Enter this key:</p>
            <p className="mt-1 font-mono text-[0.8125rem] break-all text-ink">{secret.match(/.{1,4}/g)?.join(" ")}</p>
          </div>
        </div>
        <Field
          label="Code from the app"
          name="code"
          inputMode="numeric"
          autoComplete="one-time-code"
          pattern="[0-9 ]{6,7}"
          maxLength={7}
          required
          value={code}
          onChange={(e) => setCode(e.target.value)}
          className="[&_input]:font-mono [&_input]:tracking-[0.2em]"
        />
      </fieldset>
      <SubmitButton pending="Activating…" className="w-full">
        Activate my account
      </SubmitButton>
    </form>
  );
}
