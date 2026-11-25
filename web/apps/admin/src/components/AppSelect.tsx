import { filterInput } from "./FilterBar";

type AppOption = { id: string; name: string; account_prefix: string };

export function AppSelect({ apps, value, name = "app_id" }: { apps: AppOption[]; value?: string | undefined; name?: string }) {
  return (
    <select name={name} defaultValue={value ?? ""} className={filterInput}>
      <option value="">All apps</option>
      {apps.map((a) => (
        <option key={a.id} value={a.id}>
          {a.name} ({a.account_prefix})
        </option>
      ))}
    </select>
  );
}
