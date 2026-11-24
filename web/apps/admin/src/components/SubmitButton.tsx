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
  disabled = false,
}: {
  children: ReactNode;
  pending: string;
  variant?: ButtonVariant;
  className?: string;
  disabled?: boolean;
}) {
  const { pending: busy } = useFormStatus();
  return (
    <Button type="submit" variant={variant} disabled={busy || disabled} aria-disabled={busy || disabled} className={className}>
      {busy ? pending : children}
    </Button>
  );
}
