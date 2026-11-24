export function FormError({ message }: { message?: string | undefined }) {
  if (!message) return null;
  return (
    <p role="alert" className="rounded-md border border-bad/30 bg-bad-soft px-3 py-2.5 text-sm text-bad">
      {message}
    </p>
  );
}
