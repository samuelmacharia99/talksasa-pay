import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes } from "react";

import { cx } from "./cx";

export const inputClass =
  "block w-full rounded-md border border-line-strong bg-surface px-3 h-10 text-sm text-ink placeholder:text-faint " +
  "focus:border-violet focus:outline-2 focus:outline-violet/25 aria-[invalid=true]:border-bad";

interface FieldShellProps {
  label: string;
  name: string;
  hint?: ReactNode;
  error?: string | undefined;
  className?: string;
  children: ReactNode;
}

function FieldShell({ label, name, hint, error, className, children }: FieldShellProps) {
  return (
    <div className={cx("grid gap-1.5", className)}>
      <label htmlFor={name} className="text-[0.8125rem] font-medium text-ink-2">
        {label}
      </label>
      {children}
      {error ? (
        <p id={`${name}-error`} className="text-[0.8125rem] text-bad" role="alert">
          {error}
        </p>
      ) : hint ? (
        <p id={`${name}-hint`} className="text-[0.8125rem] text-muted">
          {hint}
        </p>
      ) : null}
    </div>
  );
}

export function Field({
  label,
  name,
  hint,
  error,
  className,
  ...input
}: Omit<InputHTMLAttributes<HTMLInputElement>, "name"> & {
  label: string;
  name: string;
  hint?: ReactNode;
  error?: string | undefined;
}) {
  return (
    <FieldShell label={label} name={name} hint={hint} error={error} className={className}>
      <input
        id={name}
        name={name}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${name}-error` : hint ? `${name}-hint` : undefined}
        className={inputClass}
        {...input}
      />
    </FieldShell>
  );
}

export function SelectField({
  label,
  name,
  hint,
  error,
  className,
  children,
  ...select
}: Omit<SelectHTMLAttributes<HTMLSelectElement>, "name"> & {
  label: string;
  name: string;
  hint?: ReactNode;
  error?: string | undefined;
}) {
  return (
    <FieldShell label={label} name={name} hint={hint} error={error} className={className}>
      <select id={name} name={name} aria-invalid={error ? true : undefined} className={cx(inputClass, "pr-8")} {...select}>
        {children}
      </select>
    </FieldShell>
  );
}
