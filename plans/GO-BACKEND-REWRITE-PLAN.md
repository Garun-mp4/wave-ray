# План полной миграции backend RuView на Go

**Статус:** план согласован; реализация Go backend начата в отдельной рабочей копии
**Дата:** 2026-10-09
**Изменения кода:** начат Go runtime в `backend/`; актуальная матрица выполненного и незавершённого находится в `backend/MIGRATION_STATUS.md`.

Этот документ конкретизирует и заменяет раздел о выборе Rust/Go в `ROADMAP-BACKEND-FRONTEND-UX.md` для backend Wi-Fi sensing. Остальные, не относящиеся к backend этапы общей дорожной карты этим документом не меняются.

## 1. Цель и границы

Целевой результат — заменить на Go действующий backend системы Wi-Fi sensing: серверный процесс и все его runtime-модули, которые принимают CSI/RF данные, обрабатывают их, ведут состояние/модели/записи и обслуживают HTTP, WebSocket и UDP интерфейсы. В конечной поставке не должно быть зависимости от Rust sensing-server или Python legacy API для работы этих функций.

Миграция должна сохранить совместимость с текущими ESP32 прошивками, веб-клиентом, API, CSI-записями и моделями там, где форматы действительно поддерживаются. Перенос не должен автоматически превращать экспериментальные или синтетические результаты в подтверждённые возможности. Ошибки, обнаруженные в старом коде, исправлять осознанно: совместимые исправления покрывать тестом, а изменения wire/API контрактов версионировать и документировать.

**Предварительная граница объёма**

- **Входит:** `wifi-densepose-sensing-server` и фактическая runtime-зависимость от `wifi-densepose-core`, `signal`, `hardware`, `engine`, `worldgraph`, `rufield`, BFLD, auth, calibration, inference, model/recording/training и vendor CSI кода. Точный dependency closure подтвердить на этапе 0, а не переносить весь Cargo workspace механически.
- **Входит:** используемые сейчас sensing API и backend-возможности, доступные через `docker/docker-compose.yml`, включая уникальные возможности Python legacy API, если они всё ещё документированы/доступны пользователю. Их нужно перенести или явно вывести из продукта — не оставлять второй скрытый backend.
- **Входит:** серверная раздача существующих UI-ресурсов и API для них. Сам frontend остаётся без изменений в этой работе; его старые HTTP/WS контракты должны продолжить работать.
- **Не входит в эту sensing-миграцию:** ESP32 firmware, web/mobile frontend, браузерные WASM-пакеты, CLI/desktop разработчика, тестовые harnesses и отдельные исследовательские программы. Их протоколы/ABI могут быть потребителями, поэтому совместимость проверяется, но их язык не меняется.
- В `docker/Dockerfile.rust` дополнительно собираются отдельные `homecore-server` и `cog-ha-matter` бинарники. Это самостоятельные backend-продукты, а не ядро Wi-Fi обнаружения. На этапе 0 проверить, включены ли они в нужную пользователю поставку. Этот план не переписывает их автоматически; если требование пользователя означает все серверы всего репозитория, для них нужна отдельная migration-матрица, иначе нельзя заявлять, что весь большой workspace переписан на Go.

## 2. Что уже видно в кодовой базе

Предварительный обзор текущего дерева показывает, что это миграция алгоритмического и системного backend, а не замена web-framework:

- Production sensing crate — `v2/crates/wifi-densepose-sensing-server`. Его `src/main.rs` содержит около 15,700 строк, множество маршрутов и обработчиков; это число замерено 2026-10-09 простым подсчётом строк и включает встроенные тесты.
- Сервер принимает и обслуживает несколько классов данных: ESP32 UDP, vendor CSI/RF адаптеры, Wi-Fi источник и симулятор; имеет REST, несколько WebSocket потоков, static UI serving, auth/WS tickets, OAuth, telemetry, records/models, calibration/training, source provenance, privacy, multi-node sync, MQTT/HA/Matter и интеграции.
- Рядом с `main.rs` есть отдельные крупные модули: `training_api.rs`, `trainer.rs`, `csi.rs`, `dataset.rs`, `rvf_container.rs`, `rvf_pipeline.rs`, `vital_signs.rs`, `model_manager.rs`, CSI vendor и integration модули. Часть алгоритмов вынесена в другие крейты; их API и потребители требуют анализа.
- `docker/docker-compose.yml` по-прежнему описывает альтернативный Python `python-sensing` сервис; Python приложение находится под `archive/v1/src`. Его текущая поддержка, уникальные endpoints и расхождения с Rust надо подтвердить. Устаревшие Python планы и API-документы не считать контрактом без проверки реализации.
- `field_localize.rs` прямо описывает hotspot поля как не метрическую многопозиционную триангуляцию, с координатой на плоскости `y=0`. `pose.rs` содержит эвристическое построение скелета. Эти факты важны для parity: Go-перенос не должен утверждать точные координаты или реальную позу там, где алгоритм их не измеряет.
- Существующий Rust код включает собственные форматы и протоколы. Их нельзя заменить несовместимыми структурами Go только потому, что новое API проще написать.

Этот список — ориентир для детального dependency/feature audit, не гарантия, что каждый перечисленный feature включён в обычный runtime.

## 3. Этап 0 — зафиксировать точный объём и baseline

**Работы**

1. Проверить чистоту/состояние Git и правила проекта, собрать список реально поставляемых бинарников, entrypoints, Cargo features, Compose profiles и конфигураций запуска.
2. Построить dependency closure именно от `sensing-server` и отмеченных продуктовых функций. Для каждого Rust crate указать: используется ли при запуске; какая функция на него опирается; есть ли внешний потребитель (CLI, desktop, cog, WASM); можно ли портировать модуль независимо.
3. Снять полный перечень HTTP endpoints (method/path/status/schema/auth), заголовков, error envelopes и CORS; WebSocket handshake, события, binary/text encoding, ping/close/ticket lifecycle; UDP packet layouts, byte order, sequence/timestamp rules, guards и firmware version support.
4. Снять контракты командной строки и окружения: source selection, порты, bind addresses, модель/recording directories, privacy/auth, OAuth, feature flags и коды завершения, на которые опираются entrypoint и Compose.
5. Описать состояния хранения: `.csi.jsonl`, метаданные записей, dataset/label files, модельные/checkpoint файлы, `.rvf` контейнер, калибровка и кэш/registry. Зафиксировать поведение при перезапуске, частичном файле и ошибке диска.
6. Сравнить Rust и Python API, клиентские вызовы UI, firmware struct definitions и принятые ADR. За основу взять исполняемый код и tests, затем документы; старые README с неподтверждёнными обещаниями отдельно пометить.
7. Запустить и сохранить исходный baseline сборок/тестов по root `AGENTS.md`, доступные security checks, воспроизводимый replay benchmark и UI-to-API smoke сценарии. Отдельно зарегистрировать pre-existing failures; никаких новых benchmark claims без reproducer.
8. Подготовить нейтральные golden-fixtures без реальных CSI/person data и схему для локальных capture-файлов, которые не коммитятся. Выбрать численные допуски parity на тестовых векторах до porting, отдельно для FP32/FP64 операций.

**Артефакты / gate**

- Матрица `feature → Rust source/crate → API/WS/UDP/file contract → Go package → validation → статус (active/legacy/stub/unsupported)`.
- Compatibility pack: OpenAPI/JSON schema или эквивалент для REST, события WS, образцы UDP frame, storage/model fixtures, CLI/env reference.
- Baseline report: функциональность, ошибки, скорость/latency/throughput/RSS на одной тестовой машине и одном replay; MEASURED/CLAIMED/SYNTHETIC помечены отдельно.
- Подтверждена граница с `homecore-server`/`cog-ha-matter` и Python legacy API до реализации.

Нельзя переходить к массовому porting, пока не известны перечень поддерживаемых функций и эталонное поведение их контрактов.

## 4. Целевая Go-архитектура

Предпочтительный вариант — **один модульный Go-процесс**, а не микросервисы. Данные идут от одного/нескольких источников CSI к общей обработке и затем раздаются API/WS; разделение по процессам добавит IPC, синхронизацию, deploy/debug и больше границ отказа без требования масштаба.

Предлагаемая структура (уточнить после dependency audit, не создавать пакет для каждого файла):

```text
backend/
  go.mod
  cmd/ruview-server/main.go
  internal/
    app/             # сборка компонентов и lifecycle
    config/          # env/CLI parsing + validation
    httpapi/         # REST handlers, DTO, middleware
    ws/              # WS upgrade, tickets, subscriptions, backpressure
    ingest/          # UDP, ESP32 protocol, source guards, vendor adapters
    source/          # esp32 / host Wi-Fi / simulator selection + provenance
    signal/          # окна, фильтры, признаки и quality checks
    inference/       # модели, class/count/pose/vitals outputs
    tracking/        # temporal and multi-node state, calibration/fusion
    model/           # RVF reader/writer, metadata, registry/load lifecycle
    recording/       # CSI/dataset storage, atomic files, path safety
    auth/ privacy/ observability/ integrations/
  testdata/          # only synthetic/public/licensed fixtures without CSI PII
```

Правила реализации:

- Один Go module для backend, актуальная stable Go версия будет выбрана при старте работ и закреплена в `go.mod`, CI и Docker builder; `go.mod`/`go.sum` обновлять воспроизводимо.
- Разделять packages по реальным границам данных/изменений, использовать `internal` для implementation-only API, не создавать generic framework или интерфейс на каждый тип.
- `cmd` отвечает за parse/validation, создание зависимостей, запуск серверов и graceful shutdown; бизнес-логика не живёт в `main`/HTTP handler.
- HTTP реализовать через standard `net/http` на минимально подходящей версии Go, либо выбрать небольшой router только если выявится конкретная несовместимость/нужная функция. Маршруты и method matching тестировать явно, поскольку правила `ServeMux` менялись в Go 1.22.
- Для WebSocket выбрать поддерживаемую реализацию после аудита лицензии, CVE, протокольной совместимости, read/write concurrency и тестов; не писать RFC-совместимый WebSocket стек вручную.
- `context.Context` передавать по операциям, отменять request-scoped работу при disconnect/deadline; фоновые задачи получают owner lifecycle, сигнал stop и `WaitGroup`/эквивалент. Не оставлять unowned goroutines.
- Все очереди для CSI и subscribers ограничены, slow consumer имеет явно выбранную стратегию (drop/coalesce/disconnect) и метрику; память не растёт из-за медленного WebSocket.
- Валидация данных и конфигурации на границе; typed domain values для единиц и временных меток; ошибки имеют стабильный публичный код, внутренние детали — только в redacted logs.
- Не ослаблять auth, CSRF/Host/CORS/UDP source controls, path safety или privacy при переносе. Не включать pprof/debug endpoints на публичном интерфейсе.
- Внешние Go/C/C++ зависимости выбирать только после проверки поддержки, security policy, лицензии, платформ и реального benchmark; избегать cgo без измеренной необходимости.

## 5. Этапы портирования

### Этап 1 — каркас, конфигурация и операционный lifecycle

- Создать Go module, CI задачи и бинарь `ruview-server`; конфигурацию parse → validate → immutable runtime config.
- Реализовать signal handling, graceful HTTP/WS/UDP shutdown, единый structured logger, version/build info, health live/ready и базовые метрики.
- Сохранить CLI flags/env names/defaults/exit behavior, используемые scripts/Compose, либо оформить несовместимость заранее.
- Поднять server skeleton, который отдаёт `/health/*` и static UI, не подключая алгоритмические моки к продовой sensing логике.

**Gate:** конфигурационные и lifecycle tests; при неправильной конфигурации сервис завершается понятной ошибкой; readiness отражает готовность реального процесса.

### Этап 2 — ingress и protocol adapters

- Перенести ESP32 UDP frame decoder и все совместимые counters/guards; проверить byte order, длины, magic/version, node ids, sequence wrap, timestamp conversion и malformed/oversized packet handling.
- Портировать и выделить поддерживаемые vendor CSI/RF adapters (MediaTek, Qualcomm, Realtek и другие только если они реально включены/поддержаны).
- Перенести host Wi-Fi source adapters с платформенной честностью: Windows `netsh` path не должен объявляться доступным внутри Linux-контейнера, если исходный хостовой API там не доступен.
- Реализовать строгий source plan: `auto`, `esp32`, `wifi`, `simulated`; никакой тихой подмены реального источника синтетикой; source state/provenance распространяется до API.
- Принять firmware версии без изменения существующего wire-протокола; firmware изменение — отдельная задача, не prerequisite без доказанной нужды.

**Gate:** synthetic packet fixtures, fuzz tests decoder, replay по каждому source, проверки allowlist/invalid sources; реальное устройство проверяется отдельно на заключительном этапе.

### Этап 3 — обработка CSI и realtime state

- По dependency closure перенести core/signal алгоритмы, не копируя Rust по синтаксису: определить Go data layout, ownership и эффективные ограниченные buffers.
- Сохранить версию алгоритма и numerical semantics: нормализация, пропущенные кадры, синхронизация узлов, фильтрация фазы/амплитуды, временные окна, вычисление признаков, stale/quality state.
- Перенести adaptive classifier, presence/motion/count aggregation, multi-node fusion, synchronization, tracker and lifecycle eviction; сверить каждую часть с публичным протоколом.
- Проверить, что показатели дыхания/пульса основаны на корректных temporal windows и достаточном качестве входа. Некалиброванный сигнал не выдавать за BPM.
- Отделить сигналовый hotspot, агрегированный room-state, person track и model pose. Не портировать эвристику как доказанную точную позу/локацию; текущий status/limitations показать явно.
- Ввести явные пределы ring buffers, map lifecycle и сколько времени хранится состояние узлов/треков.

**Gate:** differential replay Rust→Go на заранее зафиксированном corpus; deterministic outputs для одинаковых входов, заданные численные допуски, отдельные tests для clock/packet loss/restart; метрики без необоснованных процентов accuracy.

### Этап 4 — модели, RVF, обучение, dataset и запись

- Специфицировать бинарный `.rvf` формат по Rust `rvf_container.rs`/`model_format.rs`: magic/version, offsets, bounds, checksums, manifest, config, weights, compatibility и rejected corruption cases.
- Реализовать Go reader/writer с golden fixtures и fuzzing; старые валидные модели читать без потери metadata/weight values. Для изменения контейнера использовать новую версию формата, не менять смысл старых полей.
- Поддержать list/load/unload/active model, load diagnostics, path confinement, atomic file writes и ограничение размера/ресурсов.
- Сопоставить доступные inference engines/модели: pure-Go реализация, portable ONNX/runtime или иная библиотека; проверить реальные weight formats, operators, CPU-only target, лицензии, memory, startup и parity до выбора. Rust FFI/child Rust binary не считать завершённой Go-миграцией.
- Перенести dataset import, labels, train/validation split, server-side adaptive training API, status/cancellation, checkpoint save/load и recording lifecycle, которые входят в активный sensing API. Offline research scripts/archive можно оставить инструментами, но не исполняемым production backend.
- Сохранить совместимость `.csi.jsonl` recordings, metadata, download/export/delete, ground truth и calibration artifacts; защитить от path traversal, partial writes, giant uploads и случайного логирования данных.

**Gate:** round-trip tests форматов, старые fixture-файлы читаются, новые записи может открыть существующий UI/tooling, training task отменяется и не блокирует stream; инференс измерен на golden inputs/held-out данных и явно маркируется по зрелости.

### Этап 5 — REST, WebSocket, безопасность и API parity

Портировать маршруты группами согласно контрактной матрице этапа 0, включая:

- health/info/status/metrics и latest sensing snapshot;
- CSI/radar/vendor event endpoints, nodes/sync/mesh, edge registry/WASM events;
- vitals, pose, stats/activity/zones, pose-physics, calibration/ground-truth;
- models, model registry/activation, recordings/datasets/adaptive training;
- `/ws/sensing`, pose stream, introspection и RuField stream;
- OAuth/resource-server verification, bearer auth, one-use/expiring WS ticket, host allowlist, privacy-mode filtering, path and payload validation;
- MQTT/Home Assistant/Matter surfaces, mDNS/discovery and optional telemetry only where their features are part of current supported runtime.

У всех маршрутов сохранить method/path, JSON field casing/defaults, status codes, auth boundary, error envelope, caching and CORS. Не полагаться на «похожий JSON»: прогонять contract tests на сериализованных ответах и frontend API клиентах. Для WebSocket проверить handshake, auth-ticket expiry/reuse, event ordering, ping/pong, disconnect cleanup, backpressure, max payload, slow client и privacy redaction.

**Gate:** все существующие frontend/backend contract scenarios работают без изменений `ui/`; security regressions tests покрывают negative cases; старый endpoint не удаляется без версии и migration note.

### Этап 6 — уникальная функциональность Python legacy и интеграций

- Сравнить legacy `archive/v1/src` и Rust sensing-server; найти уникальные routes, middleware, stream, processing и command behavior.
- Перенести только продуктово поддерживаемую уникальную функциональность и нужные contracts в Go; неподдерживаемые/stub paths пометить и удалить из пользовательских обещаний отдельным решением.
- Переключить Compose на единый Go sensing backend. Удалить Python API из нормального runtime; архивные доказательства/учебные исходники оставить архивом, если на них есть историческая необходимость.
- Убедиться, что server sidecar, subprocess и optional features не запускают Python/Rust backend скрытно.

**Gate:** repository-wide runtime audit показывает один активный Go sensing backend; не осталось UI кнопки, документа или compose-профиля, который обещает второй backend без статуса legacy.

### Этап 7 — cutover, Docker и удаление Rust sensing path

- Добавить multi-stage Go builder и minimal runtime image; оставить только требуемые системные CA/прочие необходимые библиотеки, UI assets и backend binary.
- Обновить Docker/Compose/CI: default Go service, readiness healthcheck по реальному endpoint, безопасные TCP binds, UDP source guard, `.env.example` без secret, volumes для выбранных recordings/models и explicit simulation/ESP32 profiles.
- Проверить воспроизводимость сборки, `go.sum`, SBOM/dependency review, image scanning, отсутствие Rust toolchain в final runtime и отсутствие секретов/CSI data в слоях.
- Провести shadow/differential запуск на offline replay (в отдельных портах/процессах, с deterministic fixtures); для физического UDP использовать контролируемый replay/tee и не публиковать два conflicting live answers.
- По каждому cutover шагу иметь rollback на предыдущий образ, backup моделей/записей, журнал несовместимых форматов, метрику ошибок и срок подтверждения.
- После parity и accepted cutover удалить sensing-server из default build/runtime. Удалять Rust sensing crate и его dependencies только после `cargo tree`/consumer audit; если Rust crates нужны firmware build, desktop, CLI, WASM или отдельным applications, оставить их там с объяснённой границей.

**Gate:** из корня Compose поднимает Go sensing API и UI; приём CSI, REST/WS, health readiness, stop/restart и persistence корректны; контейнер не стартует ни Rust sensing-server, ни legacy Python API.

### Этап 8 — полная приёмка и поддержка

- Go checks: `gofmt` check, `go test ./...`, `go test -race ./...`, `go vet ./...`, parser fuzzing, differential/contract/replay tests, benchmark comparison and `govulncheck`/dependency audit.
- Repo checks: все релевантные existing checks из `AGENTS.md` и CI; Python proof/tests, Rust checks where remaining Rust consumers are affected, firmware build/log where protocol/device paths change; Compose config/build and image scan.
- Hardware: ESP32-S3 (и другие поддерживаемые targets) boot/runtime log, actual CSI packet capture, node reconnect, loss/stale quality, stream to browser and clean shutdown. Наличие симулятора/зелёной сборки не считать hardware validation.
- Обновить deployment guide, env/CLI reference, API/WS/UDP contract, `.rvf` support matrix, migration/rollback notes, and limitations; обновить stale Rust/Python startup docs.
- Ревью diff: точность утверждений, security/auth, privacy, resource bounds, license/SBOM, случайные artifacts, совместимость данных и отсутствие ненужных микросервисов.

## 6. Таблица parity для Go-модуля

| Срез | Эталон | Обязательная проверка перед завершением |
|---|---|---|
| ESP32 UDP | Firmware frame structs + Rust decoder | byte-for-byte synthetic vectors, invalid lengths, sequence/time edge cases, fuzz |
| CSI processing | Rust signal functions + deterministic recordings | per-stage features within agreed tolerance, no NaN/Inf, bounded latency/RSS on replay |
| REST/WS | Rust router + current UI clients | method/path/schema/status/auth/event order parity, reconnect/slow consumer cases |
| Security | Auth/host/UDP/path/privacy middleware | unauthorized access denied; source restrictions effective; no sensitive values in logs |
| Recording/data | Existing JSONL/metadata/dataset files | old files read; Go file round-trip; interrupted writes recover or fail safely |
| Models | RVF reader/loader/inference path | file validation, metadata/weight parity, compatible output or explicit unsupported error |
| Calibration/training | Rust jobs and API contracts | deterministic state transitions, cancellation, reproducible held-out validation |
| Runtime/deploy | Current Compose/user startup | one Go backend, readiness, persistence, clean shutdown, documented host/UDP configuration |

Все допуски и нагрузочные критерии определить на этапе 0 из baseline, а не назначать после того, как увидим результат порта.

## 7. Риски и способы их снизить

| Риск | Мера |
|---|---|
| Потеря функций из-за большого и плохо обозримого server crate | Feature matrix с владельцем/contract для каждой функции; перенос вертикальными срезами; runtime audit в конце. |
| Небольшие численные отличия в DSP/ML меняют presence/vitals | Golden traces, unit normalization, explicit float tolerance, replay and ground truth; не подменять parity UI smoke-тестом. |
| Go ecosystem не имеет прямой замены конкретному Rust model/runtime или RVF loader | Ранний inference/format spike с реальными совместимыми моделями; решение до массового переноса; не оставлять Rust sidecar как «скрытую Go-миграцию». |
| Сломается совместимость firmware или frontend | Версионированный wire/API контракт и differential tests с неизменёнными firmware/UI клиентами. |
| Параллельные Rust/Go реализации расходятся | Период миграции ограничен; единый gold corpus; отдельный cutover; после принятия Go source-of-truth один. |
| Ресурсоёмкий training/inference перегружает event loop/память | Отдельные bounded workers, cancellation, admission control, limits and measured profiling; не запускать неограниченные goroutines per frame/client. |
| Реальные CSI данные случайно попадут в Git/CI | Synthetic/public/licensed fixtures only; hardware captures remain local/outside repo; проверка gitignore and artifact upload policy. |

## 8. Критерии завершения миграции

Миграция завершена, когда одновременно выполнено всё перечисленное:

1. Все согласованные активные sensing-backend features перечислены в матрице и имеют Go implementation либо явно оформлены как прекращённые/недоступные.
2. Go backend — единственный активный backend для sensing UI/API; Rust sensing binary и Python API не запускаются для этих функций.
3. ESP32 UDP, frontend REST/WS contracts, recording formats, supported model files, CLI/env and auth/privacy boundaries покрыты parity tests.
4. Relevant `go test`, race, vet, fuzz, security, performance and repo gates пройдены; исключения и pre-existing failures перечислены без маскировки.
5. Испытание с физическим target-device подтверждено runtime log/capture. Остальные режимы подписаны как simulator-only или not hardware validated.
6. Go image запускается и health/readiness проверяется через Compose; startup, configuration and rollback описаны одним источником документации.
7. В финальном code review нет непреднамеренного изменения UI/firmware, секретов/личных CSI данных, ложных точностных обещаний или незадокументированных оставшихся Rust/Python backend процессов.

## 9. Официальные ориентиры Go

При реализации сверить с текущей на тот момент документацией Go и зафиксированной версией toolchain:

- [Effective Go](https://go.dev/doc/effective_go) — стиль языка и простота concurrency patterns.
- [Go module tutorial](https://go.dev/doc/tutorial/create-module) — модуль и встроенное тестирование.
- [Go Security Best Practices](https://go.dev/doc/security/best-practices) — обновление зависимостей, fuzzing, race detector, vet и vulnerability scanning.
- [Canceling in-progress operations with context](https://go.dev/doc/database/cancel-operations) — deadlines/cancellation propagation.
- [Go Diagnostics](https://go.dev/doc/diagnostics) — profiling, tracing, runtime diagnostics.
- [Go 1.22 routing changes](https://go.dev/blog/routing-enhancements) — сверить выбранную `net/http` версию и route matching, особенно wildcard paths.

Эти документы — технические источники по Go; они не доказывают, что автоматически переписанная версия будет корректна. Корректность подтверждают только parity, security, replay и hardware validation.
