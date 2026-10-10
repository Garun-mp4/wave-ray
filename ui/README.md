# RuView operations UI

Open `http://localhost:3000/ui/`. The Go backend serves native HTML, CSS and
JavaScript modules. Running the frontend requires no Node packages or build service.

## Navigation

- **Monitor:** source provenance, current signal readings, and setup steps.
  The normalized signal grid is optional under Signal diagnostics.
- **Devices:** freshness, CSI sequence/rate, filtering; mesh timing under details.
- **Calibration:** room binding and baseline capture. CSI/trust evidence and
  count-factor controls are disclosed separately. Generated input cannot start
  a room calibration.
- **Recordings:** server frame captures and labeled activity datasets.
- **Models:** supported artifact registry and activity training; training
  parameters are collapsed by default.
- **Settings:** browser API access, connection details, optional synthetic
  fallback, and access to server integrations. Help explains setup and limits.

The old `#space` route opens Monitor. `observatory.html` and `pose-fusion.html`
redirect to Monitor; their synthetic scenes and biometric panels are retired.
Research source files remain in the repository but are not loaded by the workspace.

## Design and themes

`workspace/DESIGN.md` adapts the supplied `DESIGN-linear.md` to this product.
`design-tokens.css` owns both palettes and the local Inter Variable font.
The header sun/moon button switches themes, persists the choice, and updates
browser chrome. Before an explicit choice, the operating-system theme is used.

## Data and access

Synthetic frames are visibly labeled. Missing values are unavailable, not zero.
The current API does not provide accepted metric person coordinates, multi-person
pose or calibrated vital signs. The signal grid has no room geometry or metres.

Requests are same-origin. Tokens remain hidden and are sent in bearer headers,
never URLs. Provider credentials stay in the server environment. HTML is refreshed
from the network; only the static shell/assets support offline caching, never API data.

## Verification

```powershell
node --test ui/**/*.test.mjs
playwright-cli -s=ruview-check open http://localhost:3000/ui/
playwright-cli -s=ruview-check run-code --filename=ui/workspace/browser-check.js
playwright-cli -s=ruview-check run-code --filename=ui/workspace/browser-errors.js
playwright-cli -s=ruview-check run-code --filename=ui/workspace/browser-contrast.js
```

The browser check exercises all routes in both themes at 390, 768, 1024 and
1440 px, persistence, menu/Escape, diagnostic refresh, skip link and legacy URLs.
It writes screenshots to `.playwright-cli`; these are local review evidence.
This verifies the interface with the configured source, not ESP32 hardware accuracy.

See [backend setup](../backend/README.md) for Compose and sensor configuration.
