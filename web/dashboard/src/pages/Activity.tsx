import { useQuery } from '@tanstack/react-query';
import { api, formatDate, formatMoney } from '../lib/api';
import { Badge, Card, Empty, Failed, Loading, Mono, Table } from '../components/ui';

export function Disputes() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['disputes'],
    queryFn: () => api.disputes(),
  });

  return (
    <Card title="Disputes">
      {isLoading ? (
        <Loading />
      ) : error ? (
        <Failed error={error} />
      ) : data!.data.length === 0 ? (
        <Empty message="No disputes." />
      ) : (
        <Table headers={['Amount', 'Reason', 'Status', 'Evidence due', 'Charge']}>
          {data!.data.map((d) => {
            // Deadlines drive everything here: miss one and the dispute is
            // lost by default, whatever the evidence would have shown (§15).
            const daysLeft = Math.ceil(
              (new Date(d.evidence_due_by).getTime() - Date.now()) / 86_400_000
            );
            const urgent = d.status === 'needs_response' && daysLeft <= 3;

            return (
              <tr key={d.id} className="hover:bg-slate-50/60">
                <td className="px-5 py-3 font-medium tabular-nums text-slate-900">
                  {formatMoney(d.amount, d.currency)}
                </td>
                <td className="px-5 py-3 text-slate-600">{d.reason.replace(/_/g, ' ')}</td>
                <td className="px-5 py-3">
                  <Badge value={d.status} />
                </td>
                <td className="px-5 py-3">
                  {d.status === 'needs_response' ? (
                    <span
                      className={
                        urgent ? 'text-xs font-medium text-red-600' : 'text-xs text-slate-600'
                      }
                    >
                      {daysLeft > 0 ? `${daysLeft} days left` : 'Overdue'}
                    </span>
                  ) : (
                    <span className="text-xs text-slate-400">—</span>
                  )}
                </td>
                <td className="px-5 py-3">
                  <Mono>{d.charge_id.slice(0, 8)}</Mono>
                </td>
              </tr>
            );
          })}
        </Table>
      )}
    </Card>
  );
}

export function Payouts() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['payouts'],
    queryFn: () => api.payouts(),
  });

  return (
    <Card title="Payouts">
      {isLoading ? (
        <Loading />
      ) : error ? (
        <Failed error={error} />
      ) : data!.data.length === 0 ? (
        <Empty message="No payouts yet. The batch runs daily on settled funds." />
      ) : (
        <Table headers={['Amount', 'Status', 'Initiated', 'Paid', 'ID']}>
          {data!.data.map((p) => (
            <tr key={p.id} className="hover:bg-slate-50/60">
              <td className="px-5 py-3 font-medium tabular-nums text-slate-900">
                {formatMoney(p.amount, p.currency)}
              </td>
              <td className="px-5 py-3">
                <Badge value={p.status} />
                {p.failure_code && (
                  <div className="mt-1 text-xs text-red-600">{p.failure_code}</div>
                )}
              </td>
              <td className="px-5 py-3 text-slate-500">{formatDate(p.created_at)}</td>
              <td className="px-5 py-3 text-slate-500">
                {/* Initiated and paid are separate columns because they are
                    separate events — an ACH can be accepted and then rejected
                    days later (§18). */}
                {p.paid_at ? formatDate(p.paid_at) : <span className="text-slate-400">—</span>}
              </td>
              <td className="px-5 py-3">
                <Mono>{p.id.slice(0, 8)}</Mono>
              </td>
            </tr>
          ))}
        </Table>
      )}
    </Card>
  );
}

export function Webhooks() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['deliveries'],
    queryFn: () => api.deliveries(),
    // Deliveries retry on a backoff schedule, so a merchant watching a failing
    // endpoint wants to see it recover without reloading.
    refetchInterval: 10_000,
  });

  return (
    <Card title="Webhook deliveries">
      {isLoading ? (
        <Loading />
      ) : error ? (
        <Failed error={error} />
      ) : data!.data.length === 0 ? (
        <Empty message="No deliveries. Register an endpoint to start receiving events." />
      ) : (
        <Table headers={['Event', 'Status', 'Attempts', 'Last response', 'Next retry']}>
          {data!.data.map((d) => (
            <tr key={d.id} className="hover:bg-slate-50/60">
              <td className="px-5 py-3">
                <div className="font-medium text-slate-900">{d.event_type}</div>
                {/* The event id is what a merchant deduplicates on — delivery
                    is at-least-once, so surfacing it is part of the contract. */}
                <Mono>{d.event_id.slice(0, 8)}</Mono>
              </td>
              <td className="px-5 py-3">
                <Badge value={d.status} />
              </td>
              <td className="px-5 py-3 tabular-nums text-slate-600">{d.attempts}</td>
              <td className="px-5 py-3">
                {d.last_status_code ? (
                  <span
                    className={`text-xs tabular-nums ${
                      d.last_status_code >= 200 && d.last_status_code < 300
                        ? 'text-emerald-700'
                        : 'text-red-600'
                    }`}
                  >
                    {d.last_status_code}
                  </span>
                ) : d.last_error ? (
                  <span className="text-xs text-red-600">{d.last_error.slice(0, 40)}</span>
                ) : (
                  <span className="text-xs text-slate-400">—</span>
                )}
              </td>
              <td className="px-5 py-3 text-xs text-slate-500">
                {d.status === 'pending' && d.next_attempt_at ? (
                  formatDate(d.next_attempt_at)
                ) : d.status === 'dead' ? (
                  <span className="text-red-600">Retry budget exhausted</span>
                ) : (
                  <span className="text-slate-400">—</span>
                )}
              </td>
            </tr>
          ))}
        </Table>
      )}
    </Card>
  );
}
