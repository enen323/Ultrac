// links.ts — where a link is allowed to go. Links come out of model output,
// so the platform opener must never be handed a scheme of its choosing —
// file:, and the handler registrations a machine happens to carry, are
// reached through exactly this (plan §3.1). A structure copy of Reasonix's
// links.js: it is small, and it is the whole defence.

export function externalTarget(raw: unknown): string | null {
  let url: URL;
  try {
    url = new URL(String(raw));
  } catch {
    return null;
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return null;
  return url.toString();
}
