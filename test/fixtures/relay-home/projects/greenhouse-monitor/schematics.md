# Schematics

```
            +5V ─┬──── DHT22 VCC
                 ├──── Soil sensor VCC (x3)
                 └──── (relay coil — disabled)

            GND ─┴──── All sensors

         ESP32-S3 GPIO
           GPIO 4  ─── DHT22 DATA  (10k pull-up to +3V3)
           GPIO 34 ─── Soil 1 ADC
           GPIO 35 ─── Soil 2 ADC
           GPIO 32 ─── Soil 3 ADC
           GPIO 26 ─── Relay control (not driven)
```

## Part numbers

- ESP32-S3 dev board — generic
- DHT22 (AM2302)
- Capacitive soil sensors v1.2 (analog)
- 5V single-channel relay module (with optoisolator)

## Calibration notes

Each soil sensor reads differently. Air = ~3.0V, fully submerged ≈ 1.4V.
The `soil_pct` mapping in `firmware/main.c` linearly remaps [1.4, 3.0] →
[100, 0] — recalibrate per sensor before deploying.
