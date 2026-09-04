import { useQuery } from '@tanstack/react-query';
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { api, formatMoney } from '../lib/api';
import { Card, Failed, Loading, Stat } from '../components/ui';

export function Overview() {
  const summary = useQuery({ queryKey: ['summary'], queryFn: () => api.summary(30) });
  const volume = useQuery({ queryKey: ['volume'], queryFn: () => api.volume(30) });
  const balances = useQuery({ queryKey: ['balances'], queryFn: () => api.balances() });

  if (summary.isLoading) return <Loading />;
  if (summary.error) return <Failed error={summary.error} />;

  const s = summary.data!;
  const balance = balances.data?.data?.[0];

  return (
    <div className="space-y-6">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat
          label="Gross volume"
          value={formatMoney(s.gross_volume, s.currency)}
          hint={`${s.succeeded_count} charges · ${s.window_days} days`}
        />
        <Stat
          label="Net volume"
          value={formatMoney(s.net_volume, s.currency)}
          hint={`${formatMoney(s.refunded, s.currency)} refunded`}
        />
        <Stat
          label="Success rate"
          value={`${(s.success_rate * 100).toFixed(1)}%`}
          hint={`${s.failed_count} failed`}
        />
        {/* Amber and surfaced on the overview on purpose. A climbing count
            means the processor is unhealthy or the scheduler has stopped, and
            it is the merchant's money sitting in limbo (§24.1). */}
        <Stat
          label="Awaiting reconciliation"
          value={String(s.unresolved_count)}
          hint={s.unresolved_count > 0 ? 'Resolved by the hourly job' : 'All resolved'}
          tone={s.unresolved_count > 0 ? 'warn' : 'default'}
        />
      </div>

      {balance && (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {/* Four numbers rather than one, because "your balance" is genuinely
              ambiguous here — settled, still clearing, reserved against open
              disputes, and already sent are four different things, and a
              merchant shown only a total will plan against money they cannot
              take (§18, §19). */}
          <Stat
            label="Available"
            value={formatMoney(balance.available, balance.currency)}
            hint="Ready to pay out"
          />
          <Stat
            label="Pending"
            value={formatMoney(balance.pending, balance.currency)}
            hint="Clearing (T+2)"
          />
          <Stat
            label="Reserved"
            value={formatMoney(balance.reserved, balance.currency)}
            hint="Held against open disputes"
            tone={balance.reserved > 0 ? 'warn' : 'default'}
          />
          <Stat
            label="In transit"
            value={formatMoney(balance.in_transit, balance.currency)}
            hint="Sent, awaiting bank confirmation"
          />
        </div>
      )}

      <Card title="Volume — last 30 days">
        {volume.isLoading ? (
          <Loading />
        ) : volume.error ? (
          <Failed error={volume.error} />
        ) : (
          <div className="p-5">
            <ResponsiveContainer width="100%" height={260}>
              <AreaChart data={volume.data!.data}>
                <defs>
                  <linearGradient id="v" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="#4f46e5" stopOpacity={0.25} />
                    <stop offset="100%" stopColor="#4f46e5" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid strokeDasharray="3 3" stroke="#f1f5f9" vertical={false} />
                <XAxis
                  dataKey="date"
                  tick={{ fontSize: 11, fill: '#94a3b8' }}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(d: string) => d.slice(5)}
                />
                <YAxis
                  tick={{ fontSize: 11, fill: '#94a3b8' }}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(v: number) => `$${(v / 100).toLocaleString()}`}
                  width={70}
                />
                <Tooltip
                  formatter={(v: number) => [formatMoney(v), 'Volume']}
                  contentStyle={{
                    borderRadius: 8,
                    border: '1px solid #e2e8f0',
                    fontSize: 12,
                  }}
                />
                <Area
                  type="monotone"
                  dataKey="volume"
                  stroke="#4f46e5"
                  strokeWidth={2}
                  fill="url(#v)"
                />
              </AreaChart>
            </ResponsiveContainer>
          </div>
        )}
      </Card>
    </div>
  );
}
