import { DateTime } from "@talksasa/ui";
import type { Metadata } from "next";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { ToneLabel } from "@/components/ToneLabel";
import { actorLabel } from "@/lib/labels";
import { load, requireMe } from "@/lib/api";
import { callbackTokenLabel, credentialLabel, maskCallbackURL, SOURCE_LABEL } from "@/lib/settings";
import { callbackBaseForThisPanel } from "@/lib/settings-server";

import { PaybillForm, RegisterForm, TestForm } from "./forms";

export const metadata: Metadata = { title: "Settings" };

export default async function SettingsPage() {
  const me = await requireMe();
  const s = await load((c) => c.GET("/settings/mpesa"));
  const canManage = me.permissions.includes("settings.manage");
  const source = SOURCE_LABEL[s.source] ?? { label: s.source, help: "" };
  const reg = s.registration;
  const who = (actor: string, name: string | null) => name ?? actorLabel(actor);
  // What saving will use: the deployment's fixed URL, else this panel's address + /api/.
  const callbackBase = s.callback_base_url_fixed ? s.callback_base_url : await callbackBaseForThisPanel();

  return (
    <>
      <PageHeader
        title="Settings"
        lead="The M-Pesa paybill the whole gateway collects with. Changes apply to the api and worker within a few seconds and are recorded in the audit log."
      />
      <div className="grid gap-6">
        <Panel title="Paybill" actions={<ToneLabel label={source.label} tone={s.source === "none" ? "neutral" : s.ready ? "ok" : "warn"} />}>
          <p className="mb-4 text-sm text-ink-2">{source.help}</p>
          {s.problem && s.source !== "none" ? (
            <p role="status" className="mb-4 rounded-md border border-warn/30 bg-warn-soft px-3 py-2.5 text-sm text-warn">
              {s.problem}
            </p>
          ) : null}
          {s.updated_at ? (
            <p className="mb-4 text-[0.8125rem] text-muted">
              Version {s.version}, last changed by {who(s.updated_by ?? "", s.updated_by_name)} on <DateTime value={s.updated_at} />.
            </p>
          ) : null}
          {canManage ? (
            <PaybillForm
              current={s}
              savedEnvironment={s.source === "database" ? s.environment : null}
              environments={s.environments}
              callbackBase={callbackBase}
            />
          ) : (
            <Facts
              items={[
                ["Environment", s.environment ?? "—"],
                ["Paybill number", s.shortcode ?? "—"],
                ["STK shortcode", s.stk_shortcode ?? "Same as the paybill"],
                ["Callback base URL", s.callback_base_url ?? "—"],
                ["Consumer key", credentialLabel(s.credentials.consumer_key)],
                ["Consumer secret", credentialLabel(s.credentials.consumer_secret)],
                ["Passkey", credentialLabel(s.credentials.passkey)],
              ]}
            />
          )}
        </Panel>

        <div className="grid gap-6 lg:grid-cols-2">
          <Panel title="Connection">
            {canManage ? (
              <TestForm last={s.last_test} />
            ) : s.last_test ? (
              <p className="text-sm">
                <ToneLabel label={s.last_test.ok ? "Credentials accepted" : "Credentials refused"} tone={s.last_test.ok ? "ok" : "bad"} />{" "}
                <DateTime value={s.last_test.tested_at} />
              </p>
            ) : (
              <p className="text-sm text-muted">Not tested yet.</p>
            )}
          </Panel>

          <Panel title="Callback URLs">
            <div className="grid gap-4">
              <Facts
                items={[
                  [
                    "Confirmation URL",
                    s.callback_urls ? <span className="font-mono text-[0.8125rem]">{maskCallbackURL(s.callback_urls.confirmation)}</span> : "—",
                  ],
                  ["Callback token", callbackTokenLabel(s.callback_token_source)],
                  [
                    "Last registration",
                    reg ? (
                      <span key="r" data-testid="registration">
                        <DateTime value={reg.registered_at} /> by {who(reg.registered_by, reg.registered_by_name)}
                        {reg.current ? null : <span className="block text-warn">For an older paybill or URL: register again.</span>}
                      </span>
                    ) : (
                      "Never"
                    ),
                  ],
                ]}
              />
              {canManage ? <RegisterForm production={s.environment === "production"} disabled={!s.ready} /> : null}
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
