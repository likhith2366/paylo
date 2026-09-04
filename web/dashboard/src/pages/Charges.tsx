import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, formatDate, formatMoney } from '../lib/api';
import { Badge, Card, Empty, Failed, Loading, Mono, Table } from '../components/ui';

const FILTERS = [
  { label: 'All', value: '' },
  { label: 'Succeeded', value: 'succeeded' },
  { label: 'Failed', value: 'failed' },
  { label: 'Unresolved', value: 'requires_reconciliation' },
];

export function Charges() {
  const [status, setStatus] = useState('');
  // Cursor pagination, so pages stay stable as new charges arrive. The stack
  // keeps previous cursors so "back" works — the API only pages forward.
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors[cursors.length - 1];

  const { data, isLoading, error } = useQuery({
    queryKey: ['charges', status, cursor],
    queryFn: () => api.charges({ status: status || undefined, cursor }),
  });

  function filter(value: string) {
    setStatus(value);
    setCursors([]); // a new filter starts a new pagination sequence
  }

  return (
    <Card
      title="Charges"
      action={
        <div className="flex gap-1">
          {FILTERS.map((f) => (
            <button
              key={f.value}
              onClick={() => filter(f.value)}
              className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${
                status === f.value
                  ? 'bg-slate-900 text-white'
                  : 'text-slate-600 hover:bg-slate-100'
              }`}
            >
              {f.label}
            </button>
          ))}
        </div>
      }
    >
      {isLoading ? (
        <Loading />
      ) : error ? (
        <Failed error={error} />
      ) : data!.data.length === 0 ? (
        <Empty message="No charges yet. Create one with the API and it will appear here." />
      ) : (
        <>
          <Table headers={['Amount', 'Status', 'Card', 'Risk', 'Created', 'ID']}>
            {data!.data.map((c) => (
              <tr key={c.id} className="hover:bg-slate-50/60">
                <td className="px-5 py-3 font-medium tabular-nums text-slate-900">
                  {formatMoney(c.amount, c.currency)}
                  {/* Refunds and disputes belong on the row, not a detail page.
                      A merchant scanning for problems should see them here. */}
                  {c.amount_refunded > 0 && (
                    <span className="ml-2 text-xs font-normal text-slate-500">
                      −{formatMoney(c.amount_refunded, c.currency)} refunded
                    </span>
                  )}
                  {c.disputed && (
                    <span className="ml-2 text-xs font-medium text-red-600">disputed</span>
                  )}
                </td>
                <td className="px-5 py-3">
                  <Badge value={c.status} />
                  {c.failure_code && (
                    <div className="mt-1 text-xs text-slate-500">{c.failure_code}</div>
                  )}
                </td>
                <td className="px-5 py-3 text-slate-600">
                  {c.card_brand ? (
                    <span>
                      {c.card_brand} ····{c.card_last4}
                    </span>
                  ) : (
                    <span className="text-slate-400">—</span>
                  )}
                </td>
                <td className="px-5 py-3">
                  {c.risk_level ? (
                    <span className="flex items-center gap-2">
                      <Badge value={c.risk_level} />
                      {c.risk_score != null && (
                        <span className="text-xs tabular-nums text-slate-400">
                          {c.risk_score.toFixed(3)}
                        </span>
                      )}
                    </span>
                  ) : (
                    <span className="text-slate-400">—</span>
                  )}
                </td>
                <td className="px-5 py-3 text-slate-500">{formatDate(c.created_at)}</td>
                <td className="px-5 py-3">
                  <Mono>{c.id.slice(0, 8)}</Mono>
                </td>
              </tr>
            ))}
          </Table>

          <div className="flex items-center justify-between border-t border-slate-100 px-5 py-3">
            <button
              onClick={() => setCursors((c) => c.slice(0, -1))}
              disabled={cursors.length === 0}
              className="rounded-md px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
            >
              ← Previous
            </button>
            <span className="text-xs text-slate-400">
              Page {cursors.length + 1}
            </span>
            <button
              onClick={() =>
                data!.next_cursor && setCursors((c) => [...c, data!.next_cursor!])
              }
              disabled={!data!.has_more}
              className="rounded-md px-3 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:opacity-40 disabled:hover:bg-transparent"
            >
              Next →
            </button>
          </div>
        </>
      )}
    </Card>
  );
}
