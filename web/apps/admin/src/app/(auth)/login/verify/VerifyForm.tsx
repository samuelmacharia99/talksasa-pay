"use client";

import { Field } from "@talksasa/ui";
import { useActionState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";

import { verifyCode } from "../../actions";

export function VerifyForm() {
  const [state, action] = useActionState(verifyCode, undefined);
  return (
    <form action={action} className="mt-8 grid gap-5">
      <FormError message={state?.error} />
      <Field
        label="Authentication code"
        name="code"
        inputMode="text"
        autoComplete="one-time-code"
        maxLength={16}
        required
        autoFocus
        className="[&_input]:font-mono [&_input]:text-lg [&_input]:tracking-[0.2em]"
      />
      <SubmitButton pending="Verifying…" className="w-full">
        Sign in
      </SubmitButton>
    </form>
  );
}
