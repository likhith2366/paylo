/**
 * Typed client for the dashboard read API.
 *
 * The types below mirror the Go structs in internal/dashboard. Keeping them in
 * sync is the point: this project has twice shipped a silent contract
 * mismatch — the vault metadata fields and the ML feature vector — where both
 * sides compiled and ran while disagreeing. A renamed Go field now fails the
 * build here instead of rendering as `undefined`.
 *
 * The API key lives in localStorage for this build. Fine for a developer tool
 * run locally, NOT what a real deployment should do — a key in localStorage is
 * readable by any XSS on the page. Production would use a short-lived session
 * cookie issued by the auth service (§8), with the API key never reaching the
 * browser at all.
 */

const API_BASE = import.meta.env.VITE_API_URL ?? 'http://localhost:8080';
const KEY_STORAGE = 'paylo.api_key';

export function getApiKey(): string | null {
  return localStorage.getItem(KEY_STORAGE);
}

export function setApiKey(key: string): void {
  localStorage.setItem(KEY_STORAGE, key);
}

export function clearApiKey(): void {
  localStorage.removeItem(KEY_STORAGE);
}

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message);
  }
}

async function get<T>(path: string): Promise<T> {
  const key = getApiKey();
  if (!key) throw new ApiError(401, 'no_api_key', 'No API key configured');

  const res = await fetch(`${API_BASE}${path}`, {
    headers: { Authorization: `Bearer ${key}` },
  });

  if (!res.ok) {
    // The API returns a consistent error envelope (§22.2), but a proxy or a
    // network failure can produce something else — so parsing is guarded.
    let code = 'request_failed';
    let message = `Request failed with ${res.status}`;
    try {
      const body = await res.json();
      code = body?.error?.code ?? code;
      message = body?.error?.message ?? message;
    } catch {
      /* non-JSON response; keep the defaults */
    }
    throw new ApiError(res.status, code, message);
  }
  return res.json() as Promise<T>;
}

// --- types, mirroring internal/dashboard -----------------------------------

export interface Page<T> {
  object: string;
  data: T[];
  has_more: boolean;
  next_cursor?: string;
}

export interface Charge {
  id: string;
  amount: number;
  currency: string;
  status: 'pending' | 'succeeded' | 'failed' | 'requires_action' | 'requires_reconciliation';
  failure_code?: string;
  card_last4?: string;
  card_brand?: string;
  risk_level?: 'low' | 'medium' | 'high';
  risk_score?: number;
  amount_refunded: number;
  disputed: boolean;
  created_at: string;
}

export interface Balance {
  currency: string;
  available: number;
  pending: number;
  reserved: number;
  in_transit: number;
}

export interface Summary {
  window_days: number;
  gross_volume: number;
  net_volume: number;
  currency: string;
  succeeded_count: number;
  failed_count: number;
  success_rate: number;
  refunded: number;
  disputed_count: number;
  unresolved_count: number;
}

export interface VolumePoint {
  date: string;
  volume: number;
  count: number;
}

export interface Dispute {
  id: string;
  charge_id: string;
  amount: number;
  currency: string;
  reason: string;
  status: 'needs_response' | 'under_review' | 'won' | 'lost';
  evidence_due_by: string;
  created_at: string;
}

export interface Payout {
  id: string;
  amount: number;
  currency: string;
  status: 'pending' | 'paid' | 'failed' | 'requires_reconciliation';
  failure_code?: string;
  paid_at?: string;
  created_at: string;
}

export interface WebhookDelivery {
  id: string;
  event_id: string;
  event_type: string;
  status: 'pending' | 'delivered' | 'failed' | 'dead';
  attempts: number;
  last_status_code?: number;
  last_error?: string;
  next_attempt_at?: string;
  created_at: string;
}

// --- endpoints -------------------------------------------------------------

export const api = {
  summary: (days = 30) => get<Summary>(`/v1/dashboard/summary?days=${days}`),
  volume: (days = 30) => get<{ data: VolumePoint[] }>(`/v1/dashboard/volume?days=${days}`),
  balances: () => get<{ data: Balance[] }>('/v1/dashboard/balances'),
  charges: (params: { status?: string; cursor?: string; limit?: number } = {}) => {
    const q = new URLSearchParams();
    if (params.status) q.set('status', params.status);
    if (params.cursor) q.set('cursor', params.cursor);
    if (params.limit) q.set('limit', String(params.limit));
    return get<Page<Charge>>(`/v1/dashboard/charges?${q}`);
  },
  charge: (id: string) => get<Charge>(`/v1/dashboard/charges/${id}`),
  disputes: () => get<Page<Dispute>>('/v1/dashboard/disputes'),
  payouts: () => get<Page<Payout>>('/v1/dashboard/payouts'),
  deliveries: (status?: string) =>
    get<Page<WebhookDelivery>>(
      `/v1/dashboard/webhook_deliveries${status ? `?status=${status}` : ''}`
    ),
};

/**
 * Format minor units for display.
 *
 * Amounts are integers everywhere in this system and are divided exactly once,
 * here, at the edge, for a human to read. Nothing downstream consumes this
 * value — dividing any earlier is how float drift gets into a ledger.
 */
export function formatMoney(cents: number, currency = 'USD'): string {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency }).format(
    cents / 100
  );
}

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString('en-US', {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  });
}
