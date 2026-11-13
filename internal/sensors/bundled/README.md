# BenchPod sensor library

Sensor definitions a [BenchPod](https://embeddedci.com/benchpod) can emulate: I2C, SPI, UART and
analog sensors, each described in a plain YAML file. The pod plays the sensor to your device
under test (DUT): its registers, its commands and CRCs, its UART answers or its analog output, as
a function of values you set or a stimulus plays. The DUT runs its normal driver against it.

Every sensor here is only a definition: no firmware code. Copy one, change it, and the pod
emulates your own part.

| Path | What |
|---|---|
| `sensors/<kind>/` | one file per sensor: `i2c`, `spi`, `uart`, `analog` |
| `stimuli/` | example stimuli: values over time, with faults |
| `docs/format.md` | the format reference |
| `library.json` | the index clients read (`tools/build_library.py` writes it) |
| `tests/` | a test per sensor that reads it back the way the vendor's driver does |
| `tools/` | `validate.mjs`, `yaml2json.py`, `build_library.py`, and the pod's engine in `engine/` |

## Sensors

| Name | Kind | Part |
|---|---|---|
| `bme280` | I2C | Bosch BME280: temperature, pressure, humidity |
| `bmp280` | I2C | Bosch BMP280: temperature, pressure |
| `mpu6050` | I2C | InvenSense MPU-6050: accelerometer and gyroscope |
| `sht4x` | I2C | Sensirion SHT4x: temperature, humidity |
| `lis3dh` | SPI | ST LIS3DH: accelerometer, direct reads |
| `lis3dh_fifo` | SPI | ST LIS3DH: accelerometer, read through its FIFO |
| `gps_nmea` | UART | GPS receiver: NMEA 0183 sentences |
| `mhz19b` | UART | Winsen MH-Z19B: CO2 request and answer |
| `ntc_10k_divider` | analog | NTC 10k thermistor in a voltage divider |
| `pressure_4_20ma` | analog | 0 to 10 bar pressure transmitter, 4-20 mA |

Each file's `description` says what it covers and where the pod differs from the part (for
example, the SHT4x heater commands answer after 65.535 ms).

## Use a library sensor

Name a library entry wherever a definition is accepted.

Python SDK (`pip install embeddedci`):

```python
from embeddedci.benchpod import BenchPod

with BenchPod("usb", la_voltage=3.3) as bp:           # or a LAN address, or "embeddedci:<pod>"
    bp.sensors.start("bme280", pins={"sda": 2, "scl": 1}, values={"temperature_c": 22})
    bp.sensors.set("bme280", temperature_c=31.5)            # the DUT reads 31.5 °C now
    bp.sensors.load_stimulus("temperature_ramp_fault")      # a library stimulus
    bp.sensors.start_stimulus()
```

CLI ([benchpod-cli](https://github.com/embeddedci-com/benchpod-cli)):

```bash
benchpod sensor library                                   # the entries and the version in use
benchpod sensor start bme280 --pin sda=2 --pin scl=1 --set temperature_c=22
benchpod sensor set bme280 temperature_c=31.5
benchpod sensor stimulus load stimuli/temperature_ramp_fault.yaml
benchpod sensor stimulus start
benchpod sensor export bme280 > my_bme280.yaml            # a copy to change
```

Web app and desktop app: open the pod's **Sensors** page, pick a sensor from the library, choose
its pins and start it. Sliders set the values; a stimulus plays them over time.

## Write your own

1. Copy the closest definition, for example `sensors/i2c/bme280.yaml`, and give it a new `name`
   (the file name, at most 16 characters of `a-z`, `0-9` and `_`).
2. Change the registers, commands, messages or transfer function to your part's datasheet.
   `docs/format.md` has the whole format. Write the datasheet's decode formula and let `solve()`
   find the raw value, instead of inverting it yourself.
3. Validate it with the pod's own engine:

   ```bash
   node tools/validate.mjs my_sensor.yaml       # Node 22 or later, and python3
   benchpod sensor validate my_sensor.yaml      # or with the CLI
   ```

   The web and desktop Sensors pages check and preview it as you type.
4. Start it on the pod (`bp.sensors.start("my_sensor.yaml", ...)` or
   `benchpod sensor start my_sensor.yaml ...`) and run your DUT's driver against it.

Besides the format's limits, the pod has a few of its own, which `tools/validate.mjs` checks: an
emulated UART is 8N1; the SPI target has no data-ready pin, conversions or commands; one FIFO
sensor runs at a time; an I2C sensor has at most 4 distinct conversion or command times of at
most 65.535 ms; `excitation: adc` is not supported yet; a 16-bit register address space is one
256-byte window (`i2c.page`).

## Contribute

Pull requests are welcome. A new sensor comes with:

- the definition in `sensors/<kind>/<name>.yaml`, with `label`, `description` (what it covers,
  what it does not) and a `datasheet` link;
- a test in `tests/<name>.test.mjs` that reads the sensor back the way the vendor's driver
  does, with its own decode code (see `tests/mhz19b.test.mjs`), using Node's test runner and no
  npm packages;
- `library.json` regenerated with `python3 tools/build_library.py`.

Run the checks before you push; CI runs the same:

```bash
node tools/validate.mjs
node --test tests/*.test.mjs
python3 tools/build_library.py --check
```

## Versions

The library is released with tags `vX.Y.Z`; `CHANGELOG.md` lists what changed. Each SDK, CLI,
desktop and server release pins a library version and ships a copy of it, so a test suite never
changes under you. To use another version, set it explicitly: `latest` or a release such as
`0.2.0`, through `BENCHPOD_SENSOR_LIBRARY` (SDK), `benchpod sensor library --version latest
--save` (CLI and SDK default), or the organization setting in the web app. That version is
downloaded from its tag once and cached.

## License

Apache-2.0, see `LICENSE`.
