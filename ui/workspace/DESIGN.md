# RuView workspace design contract

## Direction

Use a quiet, dark operations-console visual language inspired by the supplied
Linear reference. RuView keeps its own name, content, and technical identity.
Prefer compact surfaces, a strong page title, readable data provenance, and
clear action hierarchy over decorative hero sections or dashboard filler.

## Tokens and layout

- Use a 4 px spacing base and the CSS custom properties in workspace.css.
- Keep the desktop navigation fixed at 240 px; content uses a 12-column grid
  capped at 1600 px. Collapse navigation to a drawer below 900 px.
- Use system sans-serif fonts and system monospace for IDs, timestamps, and
  raw numeric values. Do not download font files at runtime.
- Indigo is reserved for primary actions, links, and focus. Success, warning,
  and error use distinct semantic colors. Signal-field colors are a separate
  diagnostic scale and never identify people or health states.
- Local Lucide SVG assets share a 24 px viewBox, 2 px rounded outline, and
  current-color stroke. Use text labels for navigation and essential actions.

## Interaction and accessibility

- Every route has a clear purpose, one primary action, and meaningful empty,
  loading, offline, stale, error, disabled, and success states.
- Preserve hash routes for bookmarks and browser back/forward.
- All actions are keyboard reachable; focus is visible; controls have labels;
  status text does not depend on color alone.
- Transitions stay short and purposeful. Honor prefers-reduced-motion.
- Keep the interface usable at 390, 768, 1024, and 1440 px widths.
- Never render API strings as trusted HTML. Escape values or set them through
  textContent.

## Evidence contract

Show sensor status, source, timestamp/age, units, and calibration state when
available. Missing values are shown as unavailable, never as measured zero.
An index-space signal field is labelled as a diagnostic view, not a room map.
No person, animal, floor, track, vital-sign, or metric-coordinate marker may be
rendered without an accepted backend payload and validation evidence.

## Component states

Use shared styles for buttons, cards, tables, badges, inline alerts, progress,
dialogs/confirmations, and empty states. Destructive actions need a contextual
confirmation. Long-running jobs expose status and cancellation only when the
backend supports it.
