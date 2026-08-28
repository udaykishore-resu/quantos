import type { Metadata } from 'next';

import { DisclaimerBanner } from '@/components/disclaimer';
import { Nav } from '@/components/nav';
import { StreamStatus } from '@/components/stream-status';
import { serverEnv } from '@/lib/server/env';

import './globals.css';

export const metadata: Metadata = {
  title: {
    default: 'QuantOS — research dashboard',
    template: '%s · QuantOS',
  },
  description:
    'QuantOS is an educational research and paper-trading platform. Output is ' +
    'probabilistic, may be wrong, and is not financial advice.',
  robots: { index: false, follow: false },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const env = serverEnv();

  return (
    <html lang="en">
      <body>
        <a href="#main" className="skip-link">
          Skip to main content
        </a>

        <div className="flex min-h-screen flex-col lg:flex-row">
          <header className="shrink-0 border-b border-ink-200 bg-white lg:sticky lg:top-0 lg:h-screen lg:w-56 lg:overflow-y-auto lg:border-b-0 lg:border-r">
            <div className="flex items-baseline gap-2 px-4 py-3">
              <span className="text-base font-bold tracking-tight text-ink-900">
                QuantOS
              </span>
              <span className="rounded bg-ink-900 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-white">
                Paper
              </span>
            </div>
            <p className="px-4 pb-2 text-[11px] leading-snug text-ink-500">
              Educational research platform. No real-money orders are placed.
            </p>
            <Nav />
            {env.useMocks ? (
              <p className="mx-3 mb-3 rounded border border-watch bg-amber-50 px-2 py-1.5 text-[11px] leading-snug text-ink-800">
                <span className="font-semibold text-watch">Mock data. </span>
                Serving fixtures from <code>frontend/mocks/</code>. Nothing here
                came from a running platform.
              </p>
            ) : null}
          </header>

          <div className="flex min-w-0 flex-1 flex-col">
            <div className="no-print flex flex-wrap items-center justify-between gap-3 border-b border-ink-200 bg-white px-4 py-2">
              <StreamStatus enabled={!env.useMocks} />
              <span className="text-[11px] text-ink-500">
                API <code className="font-mono">{env.useMocks ? 'mocks/' : env.apiOrigin}</code>
              </span>
            </div>

            <main id="main" className="min-w-0 flex-1 px-4 py-5 lg:px-6">
              {children}
            </main>

            <footer className="border-t border-ink-200 bg-white px-4 py-4 lg:px-6">
              <DisclaimerBanner />
            </footer>
          </div>
        </div>
      </body>
    </html>
  );
}
