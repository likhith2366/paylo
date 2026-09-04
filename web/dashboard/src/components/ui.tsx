import type { ReactNode } from 'react';

/**
 * Status badge.
 *
 * The colour mapping is deliberate. `requires_reconciliation` is amber, not
 * red: the charge has not failed, we simply do not know yet, and showing it as
 * a failure would tell a merchant their money is gone when it may not be.
 * That distinction is the whole point of the status existing (§24.1).
 */
const TONE: Record<string, string> = {
  succeeded: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20',
  paid: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20',
  delivered: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20',
  won: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20',

  pending: 'bg-slate-100 text-slate-700 ring-slate-500/20',
  under_review: 'bg-slate-100 text-slate-700 ring-slate-500/20',

  requires_reconciliation: 'bg-amber-50 text-amber-800 ring-amber-600/20',
  requires_action: 'bg-amber-50 text-amber-800 ring-amber-600/20',
  needs_response: 'bg-amber-50 text-amber-800 ring-amber-600/20',

  failed: 'bg-red-50 text-red-700 ring-red-600/20',
  dead: 'bg-red-50 text-red-700 ring-red-600/20',
  lost: 'bg-red-50 text-red-700 ring-red-600/20',

  low: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20',
  medium: 'bg-amber-50 text-amber-800 ring-amber-600/20',
  high: 'bg-red-50 text-red-700 ring-red-600/20',
};

export function Badge({ value }: { value: string }) {
  const tone = TONE[value] ?? 'bg-slate-100 text-slate-700 ring-slate-500/20';
  return (
    <span
      className={`inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${tone}`}
    >
      {value.replace(/_/g, ' ')}
    </span>
  );
}

export function Card({
  title,
  children,
  action,
}: {
  title?: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="rounded-xl border border-slate-200 bg-white shadow-sm">
      {title && (
        <div className="flex items-center justify-between border-b border-slate-100 px-5 py-3">
          <h2 className="text-sm font-semibold text-slate-900">{title}</h2>
          {action}
        </div>
      )}
      {children}
    </div>
  );
}

export function Stat({
  label,
  value,
  hint,
  tone,
}: {
  label: string;
  value: string;
  hint?: string;
  tone?: 'default' | 'warn';
}) {
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <div className="text-xs font-medium uppercase tracking-wide text-slate-500">
        {label}
      </div>
      <div
        className={`mt-2 text-2xl font-semibold tabular-nums ${
          tone === 'warn' ? 'text-amber-700' : 'text-slate-900'
        }`}
      >
        {value}
      </div>
      {hint && <div className="mt-1 text-xs text-slate-500">{hint}</div>}
    </div>
  );
}

export function Table({
  headers,
  children,
}: {
  headers: string[];
  children: ReactNode;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-slate-100 text-left">
            {headers.map((h) => (
              <th
                key={h}
                className="px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-slate-500"
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-50">{children}</tbody>
      </table>
    </div>
  );
}

export function Empty({ message }: { message: string }) {
  return <div className="px-5 py-10 text-center text-sm text-slate-500">{message}</div>;
}

export function Loading() {
  return <div className="px-5 py-10 text-center text-sm text-slate-400">Loading…</div>;
}

/**
 * Error state.
 *
 * Shows the API's own message rather than a generic "something went wrong".
 * The API deliberately returns vague text for internal errors and specific
 * text for client errors, so passing it through is safe and far more useful
 * than replacing it.
 */
export function Failed({ error }: { error: unknown }) {
  const message = error instanceof Error ? error.message : 'Request failed';
  return (
    <div className="px-5 py-10 text-center">
      <div className="text-sm font-medium text-red-700">{message}</div>
    </div>
  );
}

export function Mono({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-xs text-slate-600">{children}</span>
  );
}
