# Go sensing backend (migration in progress)

The Go server is being migrated and is not yet the default Docker/Compose
backend. The Go SDK and module dependencies are kept in Docker for this project;
no host Go installation is required. See [MIGRATION_STATUS.md](MIGRATION_STATUS.md)
for the current feature matrix and remaining gaps.

## Run Go checks through Docker Compose

From the repository root:

```shell
docker compose -f backend/docker-compose.test.yml run --rm go-backend-tests
```

The Compose check formats Go sources, runs the package tests, and runs `go vet`.
The pinned toolchain and caches stay inside Docker; the source directory is
mounted so formatting changes remain reviewable in the checkout.

## Run the Go migration server in Docker Compose

The Go image and `docker-compose.runtime.yml` are an isolated migration
runtime; the repository's main Compose still uses Rust until API and feature
parity are accepted. From the repository root in PowerShell, select either
static-token authentication or OAuth and set the permitted UDP sender:

```powershell
$env:RUVIEW_API_TOKEN = "<long-random-token>"
$env:RUVIEW_UDP_ALLOW = "<ESP32-sender-IP-or-CIDR>"
docker compose -f backend/docker-compose.runtime.yml up --build
```

For Cognitum browser sign-in and scoped OAuth bearer tokens, configure the
issuer instead of `RUVIEW_API_TOKEN`:

```powershell
$env:RUVIEW_OAUTH_ISSUER = "https://auth.cognitum.one"
$env:RUVIEW_OAUTH_CLIENT_IDS = "ruview"
$env:RUVIEW_PUBLIC_BASE_URL = "http://127.0.0.1:3000"
$env:RUVIEW_UDP_ALLOW = "<ESP32-sender-IP-or-CIDR>"
docker compose -f backend/docker-compose.runtime.yml up --build
```

`RUVIEW_OAUTH_JWKS_URL` is optional; it defaults to the issuer's
`/.well-known/jwks.json`. `RUVIEW_PUBLIC_BASE_URL` must be the URL the browser
uses to reach this server, and its host must be included in
`SENSING_ALLOWED_HOSTS` when it is not localhost. The session-signing secret is
generated once and persisted in the Docker data volume; optionally set
`RUVIEW_SESSION_SECRET` to a shared secret of at least 32 bytes. The backend
fetches JWKS before opening listeners, so an unavailable or invalid issuer
fails startup instead of silently disabling authentication. Browser sessions
and default OAuth consent are read-only; mutations requiring `sensing:admin`
remain unavailable to those sessions. The static API token retains legacy
full-authority behavior.

The UI/API are published on loopback at `http://127.0.0.1:3000`; WebSocket is
on port `3001`, and CSI UDP on port `5005`. Go SDK and modules are downloaded
only into Docker build/container layers. The default source is `esp32`; without
a connected sensor the service reports `esp32:offline` instead of synthetic
measurements.

The migration Compose persists the MediaTek quiet-room activity baseline at
`/app/data/activity-state.json` inside the mounted data volume by default. You
can override it with `RUVIEW_ACTIVITY_STATE` or pass
`--activity-state /app/data/activity-state.json`. The Go server
restores at startup and writes an atomic, permission-restricted JSON state file
at shutdown and once per minute. The activity history itself is an in-memory
five-minute ring and is rebuilt from incoming frames after restart.

Adaptive activity models are stored at `/app/data/adaptive_model.json` in the
same persistent volume. Put labeled recordings named
`train_<class>_*.jsonl` in `/app/data/recordings`, then call the authenticated
`POST /api/v1/adaptive/train` endpoint. The model is applied to later ESP32 CSI
frames and can be checked or unloaded through `GET /api/v1/adaptive/status`
and `POST /api/v1/adaptive/unload`. The trainer accepts bounded recordings and
reports only in-sample accuracy; it is not a pretrained pose model and does
not provide person count or calibrated position.

The separate RVF model path supports the typed `ruview.activity-softmax.v2`
architecture. Its 15-feature class weights and normalization values are loaded
from the RVF manifest and applied to each new raw ESP32 CSI observation after
the signal pipeline. Models with other or missing architecture metadata remain
inspectable but are reported with `inference_available: false`; Go does not
guess how arbitrary flattened RVF weights should be executed. RSSI-derived
Wi-Fi observations are not passed to this CSI model. `GET /api/v1/models/active`
reports whether the active RVF is executable and, for trained models, its
validation metadata and live inference frame/latency counters.

Supervised RVF activity training is a background, cancellable job. Eligible
datasets are separate labelled files named `train_<class>_<recording>.jsonl`
under `/app/data/recordings`; list them with `GET /api/v1/datasets`. Each class
needs at least two distinct files. `POST /api/v1/train/start` accepts their IDs
and config (`epochs`, `batch_size`, `learning_rate`, `weight_decay`, and
`patience`); check `GET /api/v1/train/status`, watch `/ws/train/progress`, and
cancel with `POST /api/v1/train/stop`. Completed artifacts are saved under the
models directory and must be loaded through `POST /api/v1/models/load` before
they affect inference. Validation holds out whole files per class, but does
not guarantee separation by person, room, or collection session, so its score
is a diagnostic and not a deployment-quality claim. This job trains an
activity classifier only. Pose-model training, contrastive pretraining,
base-model fine-tuning, and LoRA jobs remain unavailable in Go; the current
pose model continues to use its separate experimental safetensors path.
The existing legacy TrainingPanel still records/selects ordinary `rec_*`
recordings and is not wired to this labelled-dataset contract; until the
frontend is updated, provide eligible `train_<class>_<recording>.jsonl` files
in the recordings directory and use the dataset/training API directly.

## Current sensing inputs

The Go backend accepts the existing ESP32 CSI, edge-vitals, feature-state, and
ADR-110 mesh-sync datagrams listed in `internal/protocol`. Host Wi-Fi probing is
implemented on Windows using the WLAN `netsh` interface. It reports BSSID/RSSI
observations and marks derived events with `input_mode: "rssi_derived"`; these
are not CSI measurements and do not establish metric person locations, person
counts, heart rate, or breathing rate. Host Wi-Fi scanning is unavailable in
Linux Docker containers and on macOS in this Go version. In `auto` mode, the
Windows Wi-Fi source can coexist with UDP input, and fresh ESP32 CSI takes
priority over host RSSI. Synthetic data is labeled and stops after the first
valid live sensing input.

Fresh node classifications are combined using deterministic freshness-weighted
room voting and 1.5-second hysteresis. Edge-reported counts are marked
unverified and surfaced only while the room classification is present. The
dedup factor tunes the fresh per-node count-sum fallback; this is a room-level
semantic aggregate, not spatial multi-node localization. When input stops, the
server publishes stale-node transitions to WebSocket clients without refreshing
the underlying measurement age.

Fresh raw ESP32 CSI also passes through a separate multistatic amplitude
fuser. Its result is included as `spatial_fusion` in sensing updates and is
available at `GET /api/v1/spatial-fusion/status`. The fuser resamples each
node's amplitudes to a canonical 56-tone grid, selects a bounded time-aligned
cohort, and computes an attention-weighted amplitude vector. `time_alignment`
is `mesh_epoch` only when every contributing node has a valid mesh timestamp;
otherwise the fuser uses a consistently applied `host_arrival_estimate` and
sets `synchronized` to false. ESP32 phase is node-local, so multi-node phase is
withheld unless inputs declare the same shared reference. The reported
`cross_node_coherence` is attention-weight entropy, not RF phase coherence.

Configure sensor geometry by ID with `SENSING_NODE_POSITIONS`, for example
`5:0,0,0;9:4,0,0;14:2,3,0` (coordinates are meters). The legacy positional form
`0,0,0;4,0,0` maps entries to node IDs 0 and 1; explicit IDs are recommended
for real deployments. `position_available` indicates which contributors have
configured coordinates; `geometry_configured` is true only when at least two
contributors have configured positions. Fusion does not itself locate
people: calibrated spatial localization, person count, pose, and biometric
inference do not come from this fusion result. The optional `pose_v1` path is
described below and remains experimental.

## Experimental pose inference and metric calibration

The migration image includes the repository's `pose_v1.safetensors` and
enables its Go CPU inference by default. Set `RUVIEW_POSE_MODEL_PATH` to an
empty value in the Compose environment to disable it. `RUVIEW_POSE_NODE_ID`
can select the one CSI node used by this single-sensor model; if it is unset,
inference proceeds only while exactly one fresh raw CSI node is active. With
multiple nodes, set this explicitly. RSSI-derived Wi-Fi input and CSI that does
not match the model's one-antenna/56-subcarrier input are rejected.

`GET /api/v1/pose/current` and `/api/v1/pose/frames` may return an
`experimental_pose` in `normalized_image` coordinates after 20 consecutive
frames. It is a single-person pose regressor, not a person detector: the
`persons` array remains empty, `person_detection_available` and
`multi_person_pose_available` remain false, and no per-frame confidence is
available. The stored benchmark reports PCK@20 of 3.0% and PCK@50 of 18.5%,
but does not report a mean-pose baseline. The model is therefore not accepted
for person localization or count claims.

The calibration API can fit a `pose_output_to_room_homography` from at least
six live ESP32 pose samples paired with operator-provided floor coordinates.
Capture anchors with `POST /api/v1/pose/calibrate` and
`{"action":"capture","room_x_m":1.2,"room_y_m":2.4}`, fit with
`{"action":"fit"}`, inspect `GET /api/v1/pose/calibration/status`, or clear it
with `{"action":"reset"}`. Fit uses leave-one-anchor-out error thresholds
(RMSE at most 0.5 m and maximum error at most 1 m), binds the transform to the
model SHA-256, and persists only that transform plus validation metadata with
owner-only file permissions. Ground-truth anchors are held in memory and are
not persisted. A mapped value is exposed only as
`experimental_metric_candidate`; calibration validates this transform, not
the pose model, so it does not enable `persons`, refined pose, or validated
localization. Simulation cannot be used to capture anchors. This is a
calibration prototype; reliable metric positioning and multi-person tracking
still require a suitable detector/regressor and held-out real-room validation.

## Room calibration and measurement trust

Calibration state is stored at `/app/data/room_calibration.json` in the Docker
data volume. The file contains only the room binding, model receipt, aggregate
56-tone means/variances, and dedup factor; raw CSI frames are not persisted.

Start a capture after CSI has arrived from the requested ESP32 source for at
least 10 seconds at 2 Hz or faster, with no gap of 5 seconds or more. A grid is
selected by subcarrier count and PPDU type. The room binding digest must be 64
lowercase hexadecimal characters, and device IDs must be sorted and unique:

```json
{
  "binding_digest": "<64-character lowercase SHA-256 room binding>",
  "source_node_ids": [5, 7]
}
```

Send that body to `POST /api/v1/calibration/start?source_node_id=5`. Keep the
room empty while source node 5 supplies at least 1,000 accepted frames over at
least 10 minutes. Every listed node must contribute forward frames on the
selected grid before finalization. `GET /api/v1/calibration/status` reports the
session identity, grid evidence, sequence reordering/faults, frame progress, and
missing contributors. Use the returned `boot_epoch`, `session_id`,
`binding_digest`, and `source_node_ids` unchanged in `POST
/api/v1/calibration/stop` or `/cancel`. `POST /api/v1/calibration/reset` accepts
the current `boot_epoch` alone to clear the saved model, or the complete active
identity to guard against stale requests.

`POST /api/v1/config/ground-truth` accepts `{"count": <positive integer>}` and
sets the dedup factor from fresh device-count reports. The result is persisted
and applied to the per-node count-sum fallback. `GET/POST
/api/v1/config/dedup-factor` reads or sets the factor in the range 1–10.

Calibration currently saves a baseline reference, not the Rust eigenvalue and
held-out scoring pipeline. Therefore `GET /api/v1/trust/status`,
`/api/v1/vital-signs`, and `/api/v1/edge-vitals` continue to abstain: the Go
backend does not claim calibrated person evidence, metric location, accepted
pose, heart rate, or breathing rate. The optional `pose_v1` image-space estimate is
experimental and does not qualify as accepted pose or person evidence. Numeric
biometric values and raw mmWave distance are withheld from WebSocket events
regardless of privacy mode. Bootstrap
promotion/refinement also remain unavailable.

The server's API, source selection, model/recording registry, signal processing,
mesh status, and WebSocket streaming are still a partial port. Metric
localization, pose inference, pose-model training, calibrated field scoring,
vendor radar adapters, signed RuField events, and several external integrations
have not yet been migrated. Scalar vendor RF event
decoders and REST ingestion are implemented; provider descriptors mark required
credentials, contracts, experimental capability, and unsupported devices. They
do not produce complex CSI or claim hardware validation. Separate MTC1, QCS1,
and RAC1 bridge decoders accept MediaTek, Qualcomm, and RTL8721Dx CSI frames over
the configured UDP ingress and expose bounded summaries through their `/latest`
routes. Those bridge formats are not vendor firmware ABIs, and the CSI summaries
do not infer people, pose, or metric coordinates. ADR-063 fused-vitals and
ADR-040 WASM-output packets are also decoded; WASM events are available through
`/api/v1/wasm-events` and WebSocket. Fused biometric telemetry is withheld until
the Go calibrated inference path exists. Mesh sync can recover a timestamp only
for explicitly flagged CSI frames and a fresh valid sync packet. The
multistatic amplitude fusion path does not produce calibrated person
coordinates.
`/api/v1/csi/mediatek/devices` lists fresh MediaTek snapshots; per-device
`/frames` and `/summary` routes expose a bounded CSI ring (256 frames and 512
KiB per device). `/api/v1/csi/mediatek/activity` exposes the bounded five-minute
relative channel-variation history. This is an RF activity proxy, not a
validated person count. `/api/v1/radar/latest` consumes validated RTR1
RTL8720F bridge packets and returns aggregate range/CFR summaries; raw radar
bins are not returned. These project bridge formats are not vendor firmware
ABIs and have not been validated against physical hardware.
Unsupported endpoints report an explicit unavailable state rather than
fabricating detections. The
legacy Rust RuField routes `/api/field` and `/ws/field` return `501` until the Go
trust, signature, and privacy-egress contracts are implemented.

## Vendor RF event API

`GET /api/v1/rf/vendors` returns the capability and availability descriptor for
each known provider. Bounded JSON event batches can be submitted to
`POST /api/v1/rf/vendors/{vendor}/events`; the latest accepted event per
provider is exposed at `GET /api/v1/rf/vendors/{vendor}/latest`, and the map of
latest events at `GET /api/v1/rf/vendors/latest`. When authentication is
configured, these routes require the static bearer token or an OAuth principal
with `sensing:read`.

These are scalar event adapters, not CSI decoders. Provider descriptors state
whether a contract, credentials, or experimental bridge is required; Linksys
and WiFiGarden fail closed. No vendor cloud polling client or physical vendor
hardware validation is included in this migration slice.

## Vendor CSI UDP snapshots

The common UDP listener also accepts complete MTC1 MediaTek, QCS1 Qualcomm, and
RAC1 RTL8721Dx CSI bridge datagrams. `/api/v1/csi/mediatek/latest`,
`/api/v1/csi/qualcomm/latest`, and `/api/v1/csi/realtek/latest` return one
validated metadata/amplitude summary per vendor. Latest snapshots omit raw CSI;
MediaTek inspection history is retained separately in its bounded ring. Vendor
CSI remains separate from ESP32 room processing and does not establish detection
accuracy, calibration, or real-hardware support.

## Mesh status

`GET /api/v1/mesh` returns per-node sync state, `GET /api/v1/nodes/{id}/sync`
returns one node's sync snapshot, and `GET /api/v1/mesh/metrics` exports
Prometheus metrics. Fresh valid sync packets can provide sequence-aligned
timestamps on CSI frames carrying the firmware sync-valid flag. The separate
multistatic result is exposed by `GET /api/v1/spatial-fusion/status` and in
each sensing update. It fuses amplitude views only; it does not establish
metric localization or validated person detections.

## Edge module registry

`GET /api/v1/edge/registry` fetches the configured public catalog on demand and
caches the last valid JSON for one hour. `?refresh=1` bypasses the cache. If a
refresh fails, the API returns the cached catalog with `stale: true`; with no
cache, it returns a generic `503`. Configure the mirror and cache duration with
`--edge-registry-url` and `--edge-registry-ttl-secs`, or disable it with
`--no-edge-registry` / `RUVIEW_NO_EDGE_REGISTRY=true`.
