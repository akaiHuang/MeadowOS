// Run before styles paint. This is a cosmetic preference, not account state.
try {
  document.documentElement.dataset.theme =
    localStorage.getItem('meadow.desktop.theme.v1') === 'light' ? 'light' : 'dark';
} catch {
  document.documentElement.dataset.theme = 'dark';
}
