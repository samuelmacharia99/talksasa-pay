import { filterInput } from "./FilterBar";

export function ProductSelect({ products, value }: { products: Array<{ id: string; name: string; account_prefix: string }>; value?: string | undefined }) {
  return (
    <select name="product_id" defaultValue={value ?? ""} className={filterInput}>
      <option value="">All products</option>
      {products.map((p) => (
        <option key={p.id} value={p.id}>
          {p.name} ({p.account_prefix})
        </option>
      ))}
    </select>
  );
}
