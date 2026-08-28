import type { Config } from 'tailwindcss';

/**
 * The palette is deliberately conservative. Colour is never the sole carrier of
 * meaning anywhere in this dashboard (WCAG 1.4.1): every state that uses colour
 * also carries a text label and, where it is a verdict, a glyph.
 */
const config: Config = {
  content: ['./src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: {
          50: '#f6f7f9',
          100: '#eceef2',
          200: '#d4d8e0',
          300: '#aeb5c4',
          400: '#818ba0',
          500: '#616b82',
          600: '#4c5468',
          700: '#3e4555',
          800: '#353b48',
          900: '#20242e',
          950: '#14171d',
        },
        // Verdict colours. Each is paired with a text label in the UI.
        // Named `deny` rather than `block` so the class never reads as
        // Tailwind's `block` display utility.
        allow: '#0f766e',
        watch: '#b45309',
        deny: '#b91c1c',
        up: '#0f766e',
        flat: '#525b6e',
        down: '#b91c1c',
      },
      fontFamily: {
        sans: ['ui-sans-serif', 'system-ui', '-apple-system', 'Segoe UI', 'Roboto', 'Helvetica Neue', 'Arial', 'sans-serif'],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'Liberation Mono', 'monospace'],
      },
    },
  },
  plugins: [],
};

export default config;
