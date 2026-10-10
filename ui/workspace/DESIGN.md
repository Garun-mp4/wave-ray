# Wave Ray interface design contract

## Authority

The supplied `DESIGN-linear.md` defines the visual system. This is a local RF
operations tool, with compact controls, flat sections, consistent tables and
forms. No decorative gradients, glows, cinematic scenes or invented metrics.
The existing native HTML/CSS/ES-module architecture is retained.

## Shared tokens

`design-tokens.css` owns palette, typography, radii and layout.

| Role | Dark | Light |
|---|---|---|
| Canvas | #08090a | #f7f8f8 |
| Panel | #0f1011 | #ffffff |
| Level 3 | #191a1b | #f0f1f2 |
| Secondary surface | #1f2023 | #e8e9eb |
| Primary text | #f7f8f8 | #08090a |
| Secondary text | #d0d6e0 | #2c2e33 |
| Tertiary text | #8a8f98 | #5a5f6b |
| Interactive accent | #5e6ad2 | #5e6ad2 |
| Link / accent hover | #828fff | #4b57c8 |

Borders use white overlays 2/5/8% in dark and black 3/6/10% in light.
Quaternary text is disabled/decorative only. Semantic dot colors follow the source;
semantic text uses readable theme-specific variants. Primary buttons retain white
text in both themes and use #4b57c8 on hover to maintain normal-text contrast.

- Local Inter Variable, OFL notice retained, `cv01` and `ss03` enabled.
- Weight 400 body, 510 UI emphasis, 590 headings. System monospace only for IDs,
  timestamps and raw numeric data. Body 14 px, secondary 13 px, captions 12 px.
- Spacing uses a 4 px base. Sidebar 240 px, content maximum 1280 px, 12-column grid.
- Inputs/buttons radius 6 px; cards 8 px; panels 12 px; large containers 22 px.
- Card padding 24 px desktop / 16 px mobile. Floating surfaces alone use shadows.
- Transitions name their properties, around 150 ms. No resize animation for the
  mobile drawer. Honor reduced motion; no decorative continuous animation.
- One local Lucide sprite (ISC attribution retained), consistent 24-unit viewBox
  and stroke weight. Standard header icon 16 px, target 32 px / mobile 40 px.

## Information architecture

Primary navigation: Monitor, Space, Devices, Calibration, Recordings, Models, Settings.
Help is secondary. Integrations is reached through Settings, with a return link.
Monitor leads with signal readings and a device → calibration → recording path.
Diagnostics, mesh timing, CSI/trust details and training parameters use native
keyboard-operable disclosure. Preserve expanded sections across data refreshes.

Retire synthetic Observatory/Pose Fusion entry points with redirects and a
no-JavaScript fallback link. #space shows installation geometry.
Do not show person locations, object tracking, floors, pose or biometrics without
an accepted backend payload and hardware evidence. Sample grids are diagnostics.

Space uses the shared panel surface, Inter labels and violet selection/live
indicators. No floor, grid plane, glow, animated RF range or generated objects.
Signed installation-local coordinates use Y as vertical. Device markers are
schematic symbols, not physical device dimensions. Configured routers do not
imply discovered APs or measured coverage. Pointer/keyboard orbit, pan and zoom
redraw on demand; there is no perpetual animation. A text device list provides
the same positions and states as the canvas. Layout editing collapses into the
inspector and stacks beneath the viewport on narrow screens.

## Themes and access

The header toggle names the target theme and persists the explicit selection.
System preference applies until overridden. Initialize before styles to avoid a
palette flash. Storage failures do not prevent theme switching. Synchronize tabs.
Style selection, caret, scrollbars, focus, form controls and browser theme color.

Support focus-visible, loading, empty, stale, offline, authorization, error,
success and disabled states. API text is escaped. Tokens remain hidden.
Mobile menu traps focus, closes on Escape, makes background inert, and returns
focus to its trigger. Skip navigation must not change the current route.

Verify actual pages at 390, 768, 1024 and 1440 px in both themes. Tables scroll
inside their own container; the page must not overflow horizontally. No build or
synthetic browser run establishes sensing accuracy on real hardware.
