import { useState } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { NavLink, Navigate, Route, HashRouter as Router, Routes } from 'react-router-dom';
import { clearApiKey, getApiKey, setApiKey } from './lib/api';
import { Overview } from './pages/Overview';
import { Charges } from './pages/Charges';
import { Disputes, Payouts, Webhooks } from './pages/Activity';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Financial data should not be stale for long, but nor should every tab
      // focus hammer the API. Ten seconds is short enough that a merchant
      // watching a charge land sees it, long enough to avoid a request storm.
      staleTime: 10_000,
      // A 401 means the key is wrong; retrying cannot fix it and just delays
      // the error the user needs to see.
      retry: (count, error) =>
        count < 2 && !(error instanceof Error && error.message.includes('401')),
    },
  },
});

const NAV = [
  { to: '/overview', label: 'Overview' },
  { to: '/charges', label: 'Charges' },
  { to: '/disputes', label: 'Disputes' },
  { to: '/payouts', label: 'Payouts' },
  { to: '/webhooks', label: 'Webhooks' },
];

function KeyPrompt({ onSaved }: { onSaved: () => void }) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const key = value.trim();
    // A live secret key in a browser is a serious mistake — anyone who opens
    // devtools has it. Refuse rather than warn.
    if (key.startsWith('sk_live_')) {
      setError('Never use a live key in a browser. Use a test key.');
      return;
    }
    if (!key.startsWith('sk_test_')) {
      setError('Expected a key beginning with sk_test_');
      return;
    }
    setApiKey(key);
    onSaved();
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-50 px-4">
      <form
        onSubmit={submit}
        className="w-full max-w-sm rounded-xl border border-slate-200 bg-white p-6 shadow-sm"
      >
        <h1 className="text-lg font-semibold text-slate-900">PayFlow</h1>
        <p className="mt-1 text-sm text-slate-500">
          Paste a test API key. Run <code className="text-xs">make seed</code> to create one.
        </p>
        <input
          value={value}
          onChange={(e) => {
            setValue(e.target.value);
            setError('');
          }}
          placeholder="sk_test_..."
          autoFocus
          className="mt-4 w-full rounded-lg border border-slate-300 px-3 py-2 font-mono text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
        />
        {error && <p className="mt-2 text-xs text-red-600">{error}</p>}
        <button
          type="submit"
          className="mt-4 w-full rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800"
        >
          Continue
        </button>
        <p className="mt-4 text-xs text-slate-400">
          Stored in localStorage for local development only. A real deployment
          would use a session cookie from the auth service, and the API key
          would never reach the browser.
        </p>
      </form>
    </div>
  );
}

function Shell() {
  return (
    <div className="min-h-screen bg-slate-50">
      <header className="border-b border-slate-200 bg-white">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-6 py-3">
          <div className="flex items-center gap-8">
            <span className="text-sm font-semibold text-slate-900">PayFlow</span>
            <nav className="flex gap-1">
              {NAV.map((n) => (
                <NavLink
                  key={n.to}
                  to={n.to}
                  className={({ isActive }) =>
                    `rounded-md px-3 py-1.5 text-sm font-medium transition ${
                      isActive
                        ? 'bg-slate-100 text-slate-900'
                        : 'text-slate-600 hover:bg-slate-50 hover:text-slate-900'
                    }`
                  }
                >
                  {n.label}
                </NavLink>
              ))}
            </nav>
          </div>
          <button
            onClick={() => {
              clearApiKey();
              location.reload();
            }}
            className="text-xs font-medium text-slate-500 hover:text-slate-900"
          >
            Sign out
          </button>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-6 py-8">
        <Routes>
          <Route path="/overview" element={<Overview />} />
          <Route path="/charges" element={<Charges />} />
          <Route path="/disputes" element={<Disputes />} />
          <Route path="/payouts" element={<Payouts />} />
          <Route path="/webhooks" element={<Webhooks />} />
          <Route path="*" element={<Navigate to="/overview" replace />} />
        </Routes>
      </main>
    </div>
  );
}

export default function App() {
  const [hasKey, setHasKey] = useState(() => Boolean(getApiKey()));

  if (!hasKey) return <KeyPrompt onSaved={() => setHasKey(true)} />;

  return (
    <QueryClientProvider client={queryClient}>
      <Router>
        <Shell />
      </Router>
    </QueryClientProvider>
  );
}
