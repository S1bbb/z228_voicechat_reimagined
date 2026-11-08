// SVG-иконки (24x24, currentColor).
const P = {
  hash: '<path d="M10 3 8 21M16 3l-2 18M4 8.5h17M3 15.5h17" stroke="currentColor" stroke-width="2" stroke-linecap="round" fill="none"/>',
  speaker:
    '<path d="M11 5 6 9H3v6h3l5 4V5z" fill="currentColor"/><path d="M15.5 8.5a5 5 0 0 1 0 7M18.5 5.5a9 9 0 0 1 0 13" stroke="currentColor" stroke-width="2" stroke-linecap="round" fill="none"/>',
  mic: '<rect x="9" y="2" width="6" height="12" rx="3" fill="currentColor"/><path d="M5 10v1a7 7 0 0 0 14 0v-1M12 18v4M8 22h8" stroke="currentColor" stroke-width="2" stroke-linecap="round" fill="none"/>',
  micOff:
    '<rect x="9" y="2" width="6" height="12" rx="3" fill="currentColor"/><path d="M5 10v1a7 7 0 0 0 14 0v-1M12 18v4M8 22h8" stroke="currentColor" stroke-width="2" stroke-linecap="round" fill="none"/><path d="M3 3l18 18" stroke="var(--danger)" stroke-width="2.5" stroke-linecap="round"/>',
  headphones:
    '<path d="M3 18v-6a9 9 0 0 1 18 0v6" stroke="currentColor" stroke-width="2" fill="none"/><rect x="2" y="14" width="5" height="8" rx="2" fill="currentColor"/><rect x="17" y="14" width="5" height="8" rx="2" fill="currentColor"/>',
  headphonesOff:
    '<path d="M3 18v-6a9 9 0 0 1 18 0v6" stroke="currentColor" stroke-width="2" fill="none"/><rect x="2" y="14" width="5" height="8" rx="2" fill="currentColor"/><rect x="17" y="14" width="5" height="8" rx="2" fill="currentColor"/><path d="M3 3l18 18" stroke="var(--danger)" stroke-width="2.5" stroke-linecap="round"/>',
  gear: '<path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z" fill="none" stroke="currentColor" stroke-width="2"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" fill="none" stroke="currentColor" stroke-width="1.6"/>',
  hangup:
    '<path d="M21.6 15.5c.3.3.4.8.2 1.2l-1.1 2.1c-.2.4-.7.6-1.2.5-2-.6-3.8-1.6-5.4-2.9l1-2.5c.1-.4 0-.8-.3-1.1a12 12 0 0 0-5.6 0c-.3.3-.4.7-.3 1.1l1 2.5c-1.6 1.3-3.4 2.3-5.4 2.9-.5.1-1-.1-1.2-.5l-1.1-2.1c-.2-.4-.1-.9.2-1.2 5.3-5.3 13.9-5.3 19.2 0z" fill="currentColor" transform="rotate(0 12 12) translate(0 -3)"/>',
  screen:
    '<rect x="2" y="4" width="20" height="13" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M8 21h8M12 17v4" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><path d="M12 7.5l3 3h-2v3h-2v-3H9z" fill="currentColor"/>',
  camera:
    '<rect x="2" y="6" width="14" height="12" rx="2.5" fill="none" stroke="currentColor" stroke-width="2"/><path d="M16 10.5 22 7v10l-6-3.5z" fill="currentColor"/>',
  plus: '<path d="M12 5v14M5 12h14" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/>',
  plusCircle:
    '<circle cx="12" cy="12" r="10" fill="currentColor"/><path d="M12 7v10M7 12h10" stroke="var(--bg-secondary)" stroke-width="2.2" stroke-linecap="round"/>',
  smile:
    '<circle cx="12" cy="12" r="9.5" fill="none" stroke="currentColor" stroke-width="2"/><circle cx="9" cy="10" r="1.3" fill="currentColor"/><circle cx="15" cy="10" r="1.3" fill="currentColor"/><path d="M8 14.5a5 5 0 0 0 8 0" stroke="currentColor" stroke-width="2" stroke-linecap="round" fill="none"/>',
  reply: '<path d="M10 9V5l-7 7 7 7v-4.1c5 0 8.5 1.6 11 5.1-1-5-4-10-11-11z" fill="currentColor"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16v4z" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M13.5 6.5l4 4" stroke="currentColor" stroke-width="2"/>',
  trash:
    '<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>',
  close: '<path d="M6 6l12 12M18 6 6 18" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/>',
  users:
    '<circle cx="9" cy="8" r="4" fill="currentColor"/><path d="M1.5 21a7.5 7.5 0 0 1 15 0z" fill="currentColor"/><circle cx="17" cy="7" r="3" fill="currentColor" opacity=".6"/><path d="M17.5 13a6 6 0 0 1 5 8h-4.5a9 9 0 0 0-2.5-7.5c.6-.3 1.3-.5 2-.5z" fill="currentColor" opacity=".6"/>',
  menu: '<path d="M3 6h18M3 12h18M3 18h18" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>',
  chevron: '<path d="M7 9l5 5 5-5" stroke="currentColor" stroke-width="2.2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
  crown: '<path d="M3 8l4.5 4L12 5l4.5 7L21 8l-2 11H5z" fill="currentColor"/>',
  file: '<path d="M6 2h8l5 5v15H6z" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M14 2v5h5" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/>',
  download: '<path d="M12 3v12M7 10l5 5 5-5M4 21h16" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
  signal:
    '<rect x="3" y="15" width="4" height="6" rx="1" fill="currentColor"/><rect x="10" y="10" width="4" height="11" rx="1" fill="currentColor"/><rect x="17" y="4" width="4" height="17" rx="1" fill="currentColor"/>',
  expand: '<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/>',
  chevronLeft: '<path d="M14 6l-6 6 6 6" stroke="currentColor" stroke-width="2.2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
  chevronRight: '<path d="M10 6l6 6-6 6" stroke="currentColor" stroke-width="2.2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
  up: '<path d="M7 14l5-5 5 5" stroke="currentColor" stroke-width="2.2" fill="none" stroke-linecap="round"/>',
  down: '<path d="M7 10l5 5 5-5" stroke="currentColor" stroke-width="2.2" fill="none" stroke-linecap="round"/>',
  pin: '<path d="M15 3l6 6-3 1-4 4 .5 4.5L13 20l-3.5-3.5L4 22l-.5-.5L9 16l-3.5-3.5L7 11l4.5.5 4-4z" fill="currentColor"/>',
  search: '<circle cx="10.5" cy="10.5" r="6.5" fill="none" stroke="currentColor" stroke-width="2.2"/><path d="M15.5 15.5 21 21" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/>',
  poll: '<rect x="3" y="4" width="18" height="4" rx="2" fill="currentColor" opacity=".45"/><rect x="3" y="4" width="12" height="4" rx="2" fill="currentColor"/><rect x="3" y="10" width="18" height="4" rx="2" fill="currentColor" opacity=".45"/><rect x="3" y="10" width="6" height="4" rx="2" fill="currentColor"/><rect x="3" y="16" width="18" height="4" rx="2" fill="currentColor" opacity=".45"/><rect x="3" y="16" width="15" height="4" rx="2" fill="currentColor"/>',
  play: '<circle cx="12" cy="12" r="11" fill="rgba(0,0,0,.6)"/><path d="M9.5 7.5v9l7.5-4.5z" fill="#fff"/>',
  chart: '<path d="M4 20V10M10 20V4M16 20v-7M22 20H2" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" fill="none"/>',
  heart: '<path d="M12 21s-8-5.2-8-11a4.5 4.5 0 0 1 8-2.8A4.5 4.5 0 0 1 20 10c0 5.8-8 11-8 11z" fill="currentColor"/>',
  logout: '<path d="M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
};

export function icon(name, size = 20, cls = "") {
  return `<svg class="ico ${cls}" width="${size}" height="${size}" viewBox="0 0 24 24" aria-hidden="true">${P[name] || ""}</svg>`;
}
