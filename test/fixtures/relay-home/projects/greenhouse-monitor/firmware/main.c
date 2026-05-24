// Greenhouse monitor — ESP32-S3, DHT22 + 3 soil sensors, MQTT publisher.
// Reads every 30s. Watering relay deliberately not wired here — manual only.

#include <stdio.h>
#include <stdint.h>

// Tunable thresholds — at the top of the file by convention.
static const float FROST_WARN_C       = 2.0f;
static const float HEAT_WARN_C        = 35.0f;
static const int   LOW_MOISTURE_PCT   = 25;
static const int   SAMPLE_INTERVAL_MS = 30000;

typedef struct {
    float temp_c;
    float humidity_pct;
    int   soil_pct[3];
} sample_t;

// Stubs — replaced by ESP-IDF calls on hardware.
static sample_t read_sensors(void)         { sample_t s = {0}; return s; }
static void     mqtt_publish(sample_t s)   { (void)s; }
static void     delay_ms(int ms)           { (void)ms; }

int main(void) {
    for (;;) {
        sample_t s = read_sensors();
        mqtt_publish(s);
        delay_ms(SAMPLE_INTERVAL_MS);
    }
}
