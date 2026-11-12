# BenchPod sensor library

Sensor definitions a [BenchPod](https://embeddedci.com/benchpod) can emulate: I2C, SPI, UART and
analog sensors, described in plain YAML. Copy one, change it, and emulate your own part.

- `sensors/`: one file per sensor, by kind
- `stimuli/`: example stimuli (values over time, faults)
- `docs/format.md`: the format reference
