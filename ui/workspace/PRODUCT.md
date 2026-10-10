# Wave Ray frontend product context

Wave Ray is a local operator console for inspecting the sensing server, connected
Wi-Fi/CSI devices, calibration state, recordings, and the activity-model
workflow. The primary user is a technically capable operator or researcher
setting up and evaluating a local installation.

The UI is an observability and control surface, not a validated human-tracking
or medical product. It must show the source and freshness of measurements,
distinguish synthetic data from device data, and abstain when evidence is
missing. The current backend does not provide accepted metric person tracks,
multi-person pose, or calibrated heart/breathing measurements. The interface
must not imply that those outputs are available.

The primary workflow is: connect a source, confirm fresh data, inspect devices
and synchronization, create a room-bound CSI baseline when eligible, record
labelled data, and train or load only capabilities supported by the server.
Static pages remain served by the Go backend; the current UI uses native
HTML, CSS, and ES modules without a frontend build service.

Runtime measurements are real-input-only: missing devices produce no frames,
and the browser has no simulator or synthetic fallback. Space is a configuration
view of sensor/AP positions, not an RF object detector. It displays signed
local metre coordinates without a floor. Persons and animals remain unavailable
until compatible models and accepted real-hardware evidence exist.
