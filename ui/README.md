# RuView operations UI

The primary interface is served by the Go backend at `http://localhost:3000/ui/`.
It is implemented as native HTML, CSS, and ES modules; no frontend package
installation or build step is required.

## Workspace

- **Overview** shows server health, source provenance, frame freshness, the
  signal field when present, and backend-reported capabilities.
- **Space** displays the backend's normalized diagnostic signal grid and frame
  contributors. It is index space, not a floor plan or person-location view.
- **Devices** lists node freshness, CSI sequence and rate, and mesh clock data.
- **Calibration** manages the room-bound CSI baseline, capture lifecycle,
  ground-truth count divisor, and backend trust state.
- **Recordings & data** manages server-side frame captures and activity datasets.
- **Models & training** manages registered artifacts and supported activity
  classifier jobs.
- **Integrations** shows provider descriptors and configured poller/MQTT state.
- **Settings** stores the API token in this browser origin and controls the
  optional synthetic browser fallback.
- **Help & capabilities** explains the supported data path and its limits.

The existing **Observatory** and **Pose Fusion** research pages remain linked
under Labs. They are separate legacy/research surfaces and do not count as
validated sensing output.

## Evidence and limitations

The UI distinguishes live device frames, stale data, server outages, and
synthetic frames. Missing fields stay unavailable rather than appearing as
zero. The current Go API does not provide accepted metric person coordinates,
validated multi-person pose, or calibrated heart/breathing measurements. The
Space view therefore renders only the supplied signal diagnostic and frame
contributors; it does not place people, animals, or objects in a 3D building.

API requests are same-origin only. The browser token is read from the existing
`ruview-api-token` local-storage key and sent as a bearer header; it is never
placed in URLs or rendered back into the page. Provider credentials remain in
the backend environment.

## Local checks

Run the dependency-free workspace and service-worker tests with Node.js:

```powershell
node --test ui/workspace/view-model.test.mjs ui/workspace/api.test.mjs ui/workspace/icons.test.mjs ui/sw.test.mjs ui/services/ws-ticket.test.mjs
```

For backend/container startup and hardware configuration, see
[`backend/README.md`](../backend/README.md) and the
[User Guide](../docs/user-guide.md).