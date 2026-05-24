# Greenhouse Monitor

ESP32-based environmental monitor for a hobby greenhouse. Reads
temperature, humidity, and soil moisture every 30s; publishes over MQTT
to a Raspberry Pi running the dashboard.

## Hardware

- ESP32-S3 dev board
- DHT22 (temp/humidity)
- 3× capacitive soil-moisture sensors (analog)
- 5V relay for the watering valve (currently disabled — manual only)

## Layout

- `firmware/main.c` — sensor loop + MQTT publisher.
- `schematics.md` — wiring + part numbers.

## Working agreements

- Never enable the relay automatically. Watering must be a human action.
- All numeric thresholds (low-moisture, frost-warning) are in `firmware/main.c`
  at the top of the file — easy to find and tune.
- Keep firmware binary small enough to flash over USB in < 10s; we
  iterate fast.
