"use client";

import { Field } from "@talksasa/ui";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";

import { signIn } from "../actions";

export function LoginForm() {
  const [state, action] = useActionState(signIn, undefined);
  const [email, setEmail] = useState(""); // kept after a failed attempt

  return (
    <form action={action} className="mt-8 grid gap-5">
      <FormError message={state?.error} />
      <Field
        label="Email"
        name="email"
        type="email"
        autoComplete="username"
        required
        autoFocus
        value={email}
        onChange={(e) => setEmail(e.target.value)}
      />
      <Field label="Password" name="password" type="password" autoComplete="current-password" required />
      <SubmitButton pending="Checking…" className="mt-1 w-full">
        Continue
      </SubmitButton>
    </form>
  );
}
