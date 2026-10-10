# Wave Ray

Локальное приложение для работы с реальными Wi-Fi/CSI-измерениями ESP32,
настройки сенсоров и анализа входящего сигнала. Сервер написан на Go;
интерфейс использует HTML, CSS и ES modules, светлую и тёмную темы.

## Возможности

- Мониторинг источника и свежести сигнала, устройств и синхронизации.
- Калибровка, записи, поддерживаемые сервером модели активности.
- 3D-расстановка ESP32 и точек доступа с сохранением координат на сервере.
- Только реальные входные данные: без устройств сервер ожидает кадры.

Координаты устройств задаются оператором. Проверенные координаты людей,
распознавание животных, многопользовательская поза и медицинские измерения
сейчас не доступны. [Разбор готовых моделей](docs/PRETRAINED-ESP32-MODEL-REVIEW.md).

## Запуск

Нужен Docker Desktop. Go SDK и зависимости устанавливаются внутри Docker.
Запускайте из корня проекта в PowerShell:

```powershell
$tokenBytes = New-Object byte[] 32
[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($tokenBytes)
$env:RUVIEW_API_TOKEN = [System.BitConverter]::ToString($tokenBytes).Replace("-", "").ToLowerInvariant()
$env:RUVIEW_UDP_ALLOW = "127.0.0.1"
$env:RUVIEW_UDP_PUBLISH_ADDR = "127.0.0.1"
docker compose -f docker/docker-compose.yml up -d --build
```

Откройте http://localhost:3000/ui/ и в Settings укажите токен из переменной
окружения текущей консоли. Для подключения ESP32 по LAN настройте точный
адрес отправителя и UDP-публикацию по [инструкции сервера](backend/README.md).
Не выводите токен в логи и не сохраняйте его в репозитории.

## Документация

- [Go backend и настройки](backend/README.md)
- [Интерфейс](ui/README.md)
- [Идеи логотипа](ui/brand/concepts.html)
- [Границы переименования и совместимость](docs/BRANDING.md)

## Происхождение и лицензии

Wave Ray — самостоятельная доработка [RuView](https://github.com/ruvnet/RuView).
Исходные авторские права, лицензии и ссылки на сторонние артефакты сохранены.
[Исходный README RuView](docs/upstream/RuView-README.md) описывает upstream,
включая экспериментальные продукты; его заявления не означают, что такие
возможности реализованы или проверены в нашем Go backend.
