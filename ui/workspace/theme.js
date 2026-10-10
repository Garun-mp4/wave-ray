// Runs before styles so a saved theme never flashes the opposite palette.
(() => {
  const key = 'ruview-theme';
  const valid = value => value === 'light' || value === 'dark';
  let saved;
  try { saved = localStorage.getItem(key); } catch { /* Storage can be blocked. */ }
  const media = window.matchMedia('(prefers-color-scheme: light)');
  let explicit = valid(saved);
  function apply(theme) {
    document.documentElement.classList.add('theme-changing');
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    document.querySelector('meta[name="theme-color"]')?.setAttribute('content', theme === 'light' ? '#f7f8f8' : '#08090a');
    const toggle = document.getElementById('theme-toggle');
    if (toggle) {
      const label = theme === 'light' ? 'Switch to dark theme' : 'Switch to light theme';
      toggle.setAttribute('aria-label', label);
      toggle.title = label;
      toggle.setAttribute('aria-pressed', String(theme === 'dark'));
    }
    window.dispatchEvent(new Event('ruview-theme-change'));
    window.requestAnimationFrame(() => window.requestAnimationFrame(() => {
      document.documentElement.classList.remove('theme-changing');
    }));
  }
  apply(explicit ? saved : media.matches ? 'light' : 'dark');
  document.addEventListener('DOMContentLoaded', () => {
    apply(document.documentElement.dataset.theme);
    document.getElementById('theme-toggle')?.addEventListener('click', () => {
      explicit = true;
      const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      try { localStorage.setItem(key, next); } catch { /* The current session still works. */ }
      apply(next);
    });
  });
  media.addEventListener('change', event => { if (!explicit) apply(event.matches ? 'light' : 'dark'); });
  window.addEventListener('storage', event => {
    if (event.key !== key) return;
    explicit = valid(event.newValue);
    apply(explicit ? event.newValue : media.matches ? 'light' : 'dark');
  });
})();
