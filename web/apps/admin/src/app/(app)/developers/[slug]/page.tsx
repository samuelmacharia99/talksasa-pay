import type { Metadata } from "next";
import { notFound } from "next/navigation";

import { devActions, GuidePage } from "@/components/GuidePage";
import { guideBySlug } from "@/content/developer-docs";

type Props = { params: Promise<{ slug: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const g = guideBySlug((await params).slug);
  return { title: g ? `${g.nav} · Developers` : "Developers" };
}

export default async function GuideRoute({ params }: Props) {
  const { slug } = await params;
  const guide = slug ? guideBySlug(slug) : undefined;
  if (!guide) notFound();
  return <GuidePage guide={guide} actions={devActions()} />;
}
