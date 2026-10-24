const THEME_KEY = 'meadow.desktop.theme.v1';
let initialized = false;

export function getTheme() {
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
}

export function setTheme(value, { persist = true } = {}) {
  const theme = value === 'light' ? 'light' : 'dark';
  document.documentElement.dataset.theme = theme;
  if (persist) {
    try { localStorage.setItem(THEME_KEY, theme); } catch { /* Keep usable in-memory. */ }
  }
  window.dispatchEvent(new CustomEvent('meadow:themechange', { detail: { theme } }));
  return theme;
}

export function initTheme() {
  if (initialized) return;
  initialized = true;
  let theme = getTheme();
  try { theme = localStorage.getItem(THEME_KEY) || theme; } catch { /* Private storage. */ }
  setTheme(theme, { persist: false });
  window.addEventListener('storage', (event) => {
    if (event.key === THEME_KEY || event.key === null)
      setTheme(event.newValue, { persist: false });
  });
}

initTheme();
