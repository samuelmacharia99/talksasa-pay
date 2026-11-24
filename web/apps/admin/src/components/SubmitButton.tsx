"use client";

import { Button, type ButtonVariant } from "@talksasa/ui";
import type { ReactNode } from "react";
import { useFormStatus } from "react-dom";

/** A submit button that disables itself and shows progress while the action runs. */
export function SubmitButton({
  children,
  pending,
  variant = "primary",
  className,
}: {
  children: ReactNode;
  pending: string;
  variant?: ButtonVariant;
  className?: string;
}) {
  const { pending: busy } = useFormStatus();
  return (
    <Button type="submit" variant={variant} disabled={busy} aria-disabled={busy} className={className}>
      {busy ? pending : children}
    </Button>
  );
}
