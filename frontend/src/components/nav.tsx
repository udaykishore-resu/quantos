'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';

const SECTIONS: { heading: string; items: { href: string; label: string }[] }[] = [
  {
    heading: 'Overview',
    items: [
      { href: '/dashboard', label: 'Dashboard' },
      { href: '/market', label: 'Market' },
      { href: '/news', label: 'News' },
    ],
  },
  {
    heading: 'Research',
    items: [
      { href: '/stocks', label: 'Stocks' },
      { href: '/signals', label: 'Signals' },
      { href: '/predictions', label: 'Predictions' },
      { href: '/alerts', label: 'Alerts' },
    ],
  },
  {
    heading: 'Evidence',
    items: [
      { href: '/model-health', label: 'Model health' },
      { href: '/models', label: 'Models' },
      { href: '/backtest', label: 'Backtest' },
    ],
  },
  {
    heading: 'Simulation',
    items: [
      { href: '/paper-trading', label: 'Paper trading' },
      { href: '/settings', label: 'Settings' },
    ],
  },
];

export function Nav() {
  const pathname = usePathname();

  return (
    <nav aria-label="Main" className="p-3">
      {SECTIONS.map((section) => (
        <div key={section.heading} className="mb-4">
          <h2 className="mb-1 px-2 text-[10px] font-bold uppercase tracking-widest text-ink-400">
            {section.heading}
          </h2>
          <ul>
            {section.items.map((item) => {
              const active =
                pathname === item.href || pathname.startsWith(`${item.href}/`);
              return (
                <li key={item.href}>
                  <Link
                    href={item.href}
                    aria-current={active ? 'page' : undefined}
                    className={`flex items-center gap-2 rounded px-2 py-1.5 text-sm transition-colors ${
                      active
                        ? 'bg-ink-900 font-semibold text-white'
                        : 'text-ink-700 hover:bg-ink-100'
                    }`}
                  >
                    <span
                      aria-hidden="true"
                      className={`inline-block h-1.5 w-1.5 rounded-full ${
                        active ? 'bg-white' : 'bg-ink-300'
                      }`}
                    />
                    {item.label}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}
