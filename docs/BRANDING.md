# Wave Ray

The current product name is **Wave Ray** (two words, title case).
It appears in the application shell, page titles, PWA manifest, offline messages,
server identity, discovery display name, MQTT display labels and current product documentation.

Compatibility identifiers retain their existing spelling: `RUVIEW_*` environment
variables, Go import paths, executable paths, REST/WS schemas, mDNS service type,
MQTT topics/device identifiers, browser storage keys and named Docker volumes.
Renaming these identifiers would be a separate migration and must preserve data
and existing device/auth configuration. They are not the displayed product name.

Upstream history, published package names, firmware contracts, licenses, external
URLs, ADRs and upstream documentation retain their original attribution.
`docs/upstream/RuView-README.md` preserves the original root README; it is not the
capability description of Wave Ray.

## Approved logo

The approved mark is **Signal intersection** (`ui/brand/logo.svg`), selected by
the user on 10 October 2026. Its paths match the approved intersection concept.
Use the complete mark without glows, gradients or additional arcs.

The interface uses the SVG at 24 px; the favicon adapts to the browser theme.
PNG exports are in `ui/brand/png/` and downloadable at `/ui/brand/`.
Transparent dark-ink files are for light surfaces, and light-ink files for dark
surfaces. Wordmarks use bundled Inter Variable at weight 590.

Application icons have a solid design-system background. The maskable icon keeps
all artwork within the central safe circle (radius 40% of the canvas). Apple
Touch icons have no baked rounded corners.

To reproduce PNGs, start the app and run the existing Playwright CLI from the
project root: `playwright-cli open http://localhost:3000/ui/`, followed by
`playwright-cli run-code --filename=ui/brand/export-logos.js`.
No new dependency or host Go installation is required. Other concepts remain
as design history.
