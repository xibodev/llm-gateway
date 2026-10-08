import type { ComponentChildren } from "preact";
import { useId, useState } from "preact/hooks";
import { ArrowDown, ArrowUp, ArrowUpDown, Check, Copy } from "lucide-preact";
import { Pager } from "./ModelPicker";

export type SortState = { id: string; descending: boolean };

// A TableColumn is one column of a table: its header, which also labels each
// of its cells when a narrow screen stacks a row, its cell, and whether it
// sorts. A column the console sorts gives the value it sorts by; one the
// server sorts is sortable, and its ID is the order the server is asked for.
// headerCell, when set, is shown in the header instead of its text.
export type TableColumn<T> = {
  id: string;
  header: string;
  headerCell?: ComponentChildren;
  cell: (row: T) => ComponentChildren;
  sortValue?: (row: T) => string | number;
  sortable?: boolean;
  class?: string;
};

// A TableView is the rows a table shows on its current page, and how the
// user moves through them. useTableView sorts and pages rows the console
// holds; useServerTableView pages rows the server sorts and pages.
export type TableView<T> = {
  rows: T[];
  total: number;
  page: number;
  pageSize: number;
  pageSizes: number[];
  sort: SortState | null;
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
  toggleSort: (id: string) => void;
};

export const tablePageSizes = [25, 50, 100];

// nextSort is the order a press on a column's header asks for: a new column
// sorts ascending, a second press descending, and a third restores the
// list's own order.
export function nextSort(current: SortState | null, id: string): SortState | null {
  if (current?.id !== id) return { id, descending: false };
  return current.descending ? null : { id, descending: true };
}

function compareValues(a: string | number, b: string | number): number {
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}

// sortRows sorts rows by value, keeping rows of equal value in their order.
export function sortRows<T>(rows: T[], value: (row: T) => string | number, descending: boolean): T[] {
  return [...rows].sort((a, b) => (descending ? -1 : 1) * compareValues(value(a), value(b)));
}

// useTableView sorts and pages rows the console holds. A page past the end,
// as a narrower filter leaves, shows the last page instead.
export function useTableView<T>(rows: T[], columns: TableColumn<T>[], initialSort: SortState | null = null, pageSizes = tablePageSizes): TableView<T> {
  const [sort, setSort] = useState<SortState | null>(initialSort);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(pageSizes[0]);
  const column = sort ? columns.find((candidate) => candidate.id === sort.id) : undefined;
  const sorted = sort && column?.sortValue ? sortRows(rows, column.sortValue, sort.descending) : rows;
  const current = Math.min(page, Math.max(0, Math.ceil(sorted.length / pageSize) - 1));
  return {
    rows: sorted.slice(current * pageSize, (current + 1) * pageSize),
    total: sorted.length, page: current, pageSize, pageSizes, sort,
    setPage,
    setPageSize: (size) => { setPageSize(size); setPage(0); },
    toggleSort: (id) => { setSort((value) => nextSort(value, id)); setPage(0); },
  };
}

// ServerTableState is where a table the server pages stands: the page, its
// size and the order to ask the server for.
export type ServerTableState = { page: number; pageSize: number; sort: SortState | null };

// serverTableView is the view of one page of rows the server answered, of
// total rows, for a table whose state moves through setState.
export function serverTableView<T>(rows: T[], total: number, state: ServerTableState, setState: (next: ServerTableState) => void, pageSizes = tablePageSizes): TableView<T> {
  return {
    rows, total, page: state.page, pageSize: state.pageSize, pageSizes, sort: state.sort,
    setPage: (page) => setState({ ...state, page }),
    setPageSize: (pageSize) => setState({ ...state, pageSize, page: 0 }),
    toggleSort: (id) => setState({ ...state, sort: nextSort(state.sort, id), page: 0 }),
  };
}

// dataTable renders view as a table of columns: headers that stay in view
// and sort, rows that a narrow screen stacks with each cell labelled, and
// paging with a choice of page size once the rows outgrow the smallest. It
// is a render function, not a component, so a page's rows stay in the
// page's own element tree.
export function dataTable<T>(view: TableView<T>, columns: TableColumn<T>[], options: {
  label: string;
  rowKey: (row: T) => string;
  class?: string;
  rowClass?: (row: T) => string | undefined;
}) {
  return <div class="data-table">
    <div class="table-wrap data-table__scroll">
      <table class={options.class} aria-label={options.label}>
        <thead><tr>{columns.map((column) => {
          const sortable = Boolean(column.sortValue || column.sortable);
          const order = view.sort?.id === column.id ? (view.sort.descending ? "descending" : "ascending") : undefined;
          return <th key={column.id} scope="col" aria-sort={sortable ? order ?? "none" : undefined}>
            {column.headerCell ?? (sortable
              ? <button class="data-table__sort" type="button" title={`Sort by ${column.header.toLowerCase()}`} onClick={() => view.toggleSort(column.id)}>{column.header}{order === "ascending" ? <ArrowUp size={12} aria-hidden="true" /> : order === "descending" ? <ArrowDown size={12} aria-hidden="true" /> : <ArrowUpDown size={12} aria-hidden="true" />}</button>
              : column.header)}
          </th>;
        })}</tr></thead>
        <tbody>{view.rows.map((row) => <tr key={options.rowKey(row)} class={options.rowClass?.(row)}>{columns.map((column) => <td key={column.id} class={column.class} data-label={column.header}>{column.cell(row)}</td>)}</tr>)}</tbody>
      </table>
    </div>
    {tableFooter(view)}
  </div>;
}

// tableFooter renders a view's paging, with a choice of page size, once its
// rows outgrow the smallest. Tiles a view pages use it too.
export function tableFooter<T>(view: TableView<T>, sizeLabel = "Rows per page") {
  if (view.total <= view.pageSizes[0]) return null;
  return <footer class="data-table__footer">
    <label>{sizeLabel}<select value={String(view.pageSize)} onChange={(event) => view.setPageSize(Number((event.currentTarget as HTMLSelectElement).value))}>{view.pageSizes.map((size) => <option key={size} value={String(size)}>{size}</option>)}</select></label>
    <Pager total={view.total} page={view.page} pageSize={view.pageSize} onPage={view.setPage} />
  </footer>;
}

// shortID shows a long identifier by its two ends.
export function shortID(id: string): string {
  return id.length > 18 ? `${id.slice(0, 10)}…${id.slice(-4)}` : id;
}

// ShortID shows a long identifier by its ends, whole on hover, with a button
// that copies it whole.
export function ShortID({ id, label = "ID" }: { id: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(id);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch { /* No clipboard: the whole ID shows on hover. */ }
  };
  return <span class="short-id"><code class="technical" title={id}>{shortID(id)}</code><button class="icon-button icon-button--compact" type="button" aria-label={`Copy ${label} ${id}`} title={copied ? "Copied" : `Copy ${label}`} onClick={() => void copy()}>{copied ? <Check size={13} /> : <Copy size={13} />}</button></span>;
}

export type PickerOption = { value: string; label: string; disabled?: boolean };

// SearchSelect is a select whose options a search narrows once there are
// more than searchAt of them. The chosen option stays listed whatever the
// search, so narrowing never changes the choice.
export function SearchSelect({ label, value, options, onChange, disabled = false, searchAt = 8, noun = "options", class: className }: {
  label: string; value: string; options: PickerOption[]; onChange: (value: string) => void;
  disabled?: boolean; searchAt?: number; noun?: string; class?: string;
}) {
  const [search, setSearch] = useState("");
  const id = useId();
  const needle = search.trim().toLowerCase();
  const shown = needle ? options.filter((option) => option.value === value || option.label.toLowerCase().includes(needle)) : options;
  const select = <select id={id} value={value} disabled={disabled} aria-label={label} onChange={(event) => onChange((event.currentTarget as HTMLSelectElement).value)}>{shown.map((option) => <option key={option.value} value={option.value} disabled={option.disabled}>{option.label}</option>)}</select>;
  if (options.length <= searchAt) return <label class={className}>{label}{select}</label>;
  return <label class={[className, "search-select"].filter(Boolean).join(" ")}>{label}
    <input type="search" value={search} disabled={disabled} placeholder={`Search ${options.length} ${noun}`} aria-label={`Search ${noun}`} aria-controls={id} onInput={(event) => setSearch((event.currentTarget as HTMLInputElement).value)} />
    {select}
  </label>;
}
