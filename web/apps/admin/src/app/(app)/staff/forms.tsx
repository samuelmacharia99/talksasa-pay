"use client";

import { Button, Field, SecretOnce, SelectField } from "@talksasa/ui";
import { useActionState, useState } from "react";

import { FormError } from "@/components/FormError";
import { SubmitButton } from "@/components/SubmitButton";
import { ROLE } from "@/lib/labels";

import { changeRole, disableStaff, inviteStaff, type LinkResult, resetStaff } from "./actions";

function RoleOptions() {
  return (
    <>
      {Object.entries(ROLE).map(([v, r]) => (
        <option key={v} value={v}>
          {r.label}: {r.can}
        </option>
      ))}
    </>
  );
}

export function InviteForm() {
  const [state, action] = useActionState(inviteStaff, undefined as LinkResult);
  const [again, setAgain] = useState(0);
  if (state?.ok && again === 0) {
    return (
      <div className="grid gap-3">
        <SecretOnce
          title={`Setup link for ${state.data.name}`}
          help="Send it to them privately (not in a group chat). It works once and expires in 24 hours."
          value={state.data.url}
        />
        <div>
          <Button variant="secondary" onClick={() => setAgain(1)}>
            Invite someone else
          </Button>
        </div>
      </div>
    );
  }
  return (
    <form action={action} key={again} className="grid gap-4 sm:grid-cols-2">
      <div className="sm:col-span-2">
        <FormError message={state && !state.ok ? state.error : undefined} />
      </div>
      <Field label="Full name" name="name" required maxLength={100} autoComplete="off" />
      <Field label="Work email" name="email" type="email" required autoComplete="off" />
      <SelectField label="Role" name="role" required defaultValue="viewer" className="sm:col-span-2">
        <RoleOptions />
      </SelectField>
      <div className="sm:col-span-2">
        <SubmitButton pending="Creating link…">Create setup link</SubmitButton>
      </div>
    </form>
  );
}

export function RoleForm({ id, current }: { id: string; current: string }) {
  const [state, action] = useActionState(changeRole.bind(null, id), undefined);
  return (
    <form action={action} className="flex flex-wrap items-center gap-2">
      <label className="sr-only" htmlFor={`role-${id}`}>
        Role
      </label>
      <select
        id={`role-${id}`}
        name="role"
        defaultValue={current}
        className="h-8 rounded-md border border-line-strong bg-surface px-2 text-[0.8125rem]"
      >
        {Object.entries(ROLE).map(([v, r]) => (
          <option key={v} value={v}>
            {r.label}
          </option>
        ))}
      </select>
      <SubmitButton variant="secondary" pending="Saving…" className="h-8 px-3 text-[0.8125rem]">
        Change
      </SubmitButton>
      {state && !state.ok ? <span className="text-[0.8125rem] text-bad">{state.error}</span> : null}
    </form>
  );
}

export function AccountActions({ id, name, status }: { id: string; name: string; status: string }) {
  const [resetState, resetAction] = useActionState(resetStaff.bind(null, id), undefined as LinkResult);
  const [disableState, disableAction] = useActionState(disableStaff.bind(null, id), undefined);
  const [confirm, setConfirm] = useState<"reset" | "disable" | null>(null);

  if (resetState?.ok) {
    return (
      <SecretOnce
        title={`New setup link for ${name}`}
        help="Their password, authenticator and recovery codes were cleared and they were signed out. Send this link privately."
        value={resetState.data.url}
      />
    );
  }
  const error = (resetState && !resetState.ok && resetState.error) || (disableState && !disableState.ok && disableState.error);
  return (
    <div className="grid gap-2">
      {error ? <p className="text-[0.8125rem] text-bad">{error}</p> : null}
      {confirm === "reset" ? (
        <form action={resetAction} className="flex flex-wrap items-center gap-2">
          <span className="text-[0.8125rem] text-ink-2">Clear their password and 2FA, sign them out, and issue a new link?</span>
          <SubmitButton variant="danger" pending="Resetting…" className="h-8 px-3 text-[0.8125rem]">
            Reset
          </SubmitButton>
          <Button size="sm" variant="ghost" onClick={() => setConfirm(null)}>
            Cancel
          </Button>
        </form>
      ) : confirm === "disable" ? (
        <form action={disableAction} className="flex flex-wrap items-center gap-2">
          <span className="text-[0.8125rem] text-ink-2">Sign {name} out everywhere and block sign-in?</span>
          <SubmitButton variant="danger" pending="Disabling…" className="h-8 px-3 text-[0.8125rem]">
            Disable
          </SubmitButton>
          <Button size="sm" variant="ghost" onClick={() => setConfirm(null)}>
            Cancel
          </Button>
        </form>
      ) : (
        <div className="flex flex-wrap gap-1">
          <Button size="sm" variant="ghost" onClick={() => setConfirm("reset")}>
            {status === "disabled" ? "Re-invite" : "Reset sign-in"}
          </Button>
          {status !== "disabled" ? (
            <Button size="sm" variant="danger" onClick={() => setConfirm("disable")}>
              Disable
            </Button>
          ) : null}
        </div>
      )}
    </div>
  );
}
