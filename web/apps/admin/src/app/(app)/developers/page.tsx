import type { Metadata } from "next";

import { devActions, GuidePage } from "@/components/GuidePage";
import { guideBySlug } from "@/content/developer-docs";

export const metadata: Metadata = { title: "Developers" };

export default function DevelopersHome() {
  const guide = guideBySlug("")!;
  return (
    <GuidePage
      guide={guide}
      lead="How an app connects to Talksasa Pay. Share these guides with the app's developers; their API key comes from the app's page."
      actions={devActions()}
    />
  );
}
