"use client";

import { Field } from "@talksasa/ui";
import { useActionState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";

import { changePassword } from "./actions";

export function PasswordForm() {
  const [state, action] = useActionState(changePassword, undefined);
  return (
    <form action={action} className="grid gap-4">
      <FormError message={state && !state.ok ? state.error : undefined} />
      <Field label="Current password" name="current_password" type="password" autoComplete="current-password" required />
      <Field label="New password" name="new_password" type="password" autoComplete="new-password" minLength={12} required hint="At least 12 characters." />
      <Field label="Repeat new password" name="confirm" type="password" autoComplete="new-password" minLength={12} required />
      <p className="text-[0.8125rem] text-muted">You’ll be signed out everywhere and asked to sign in again.</p>
      <div>
        <SubmitButton pending="Changing…">Change password</SubmitButton>
      </div>
    </form>
  );
}
