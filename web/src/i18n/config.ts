export const DEFAULT_LOCALE = "zh-CN";
export const SUPPORTED_LOCALES = [DEFAULT_LOCALE, "en", "pt-BR"] as const;
export type AppLocale = (typeof SUPPORTED_LOCALES)[number];
export const LOCALE_STORAGE_KEY = "artex_locale";

export function resolveLocale(value: string | null | undefined): AppLocale {
  const raw = value ?? "";
  // hifen e o padrao BCP-47; underscore aparece em header legado e em
  // navigator.language de alguns ambientes
  if (/^pt(?:[-_]|$)/i.test(raw)) return "pt-BR";
  return /^en(?:[-_]|$)/i.test(raw) ? "en" : DEFAULT_LOCALE;
}
