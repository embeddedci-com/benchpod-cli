# BenchPod sensor definition format, version 1

A sensor definition describes a sensor so a BenchPod can play it: what the DUT sees on the bus or
on an analog input, as a function of the values you set. A **stimulus** describes how those values
change during a test, and **faults** describe what goes wrong and when. Both are separate files,
so a library sensor never carries test data.

Files are YAML (`.yaml`). JSON with the same structure is accepted everywhere; the pod itself only
receives JSON (`tools/yaml2json.py` converts). Unknown keys are errors, so a typo never goes unnoticed.
Read YAML with the 1.2 rules: only `true` and `false` are booleans, so a key such as `on:` stays the
word "on" (YAML 1.1 readers turn it into a boolean).

## 1. Common header

```yaml
format: 1                   # this document
kind: i2c                   # i2c | spi | uart | analog
name: bmp280                # [a-z0-9_]{1,32}; unique within a library
label: "Bosch BMP280: temperature and pressure"
description: |              # optional, Markdown
  Forced and normal mode, fixed factory trim.
datasheet: https://...      # optional
version: 1.0.0              # optional, the definition's own version
license: Apache-2.0         # optional
params:                     # the values a user, a stimulus or a test sets
  - { key: temperature_c, unit: C, min: -100, max: 150, default: 25, label: Temperature }
  - { key: pressure_pa, unit: Pa, min: 1000, max: 200000, default: 101325 }
constants:                  # optional, evaluated once when the sensor starts
  T1: 27504
functions:                  # optional, see 2.4
  temp_c:
    args: [adc_t]
    let:
      - var1: "(((adc_t >> 3) - (T1 << 1)) * T2) >> 11"
    return: "..."
let:                        # optional, evaluated in order on every rebuild
  - adc_t: "solve(temp_c, temperature_c, 0, 0xFFFFF)"
```

Limits per definition: 16 params, 64 constants, 16 functions, 16 fragments, 64 `let` entries,
16 states, 255 register and command entries, 32 `on_write` rules, 32 messages, 32 requests,
32 KB of JSON. A function has at most 8 arguments and 16 `let` entries; an expression or template
at most 1024 characters, nested at most 20 deep. The compiled definition must fit 8 KB, and one
rebuild, epoch or answer at most 200000 evaluation steps. The pod reports the limit that was hit.

`constants` are numbers, strings, or expressions of numbers, earlier constants and built-in
functions. Params are in `params` order; a param without `default` starts at 0 (or the end of its
range nearest 0).

## 2. Expressions

Expressions are strings (a bare number is also accepted). They are evaluated with 64-bit floats.

### 2.1 Values

- Numbers: `25`, `-1.5`, `1e5`, `0x58`, `0b1010`.
- Names: params, constants, `let` entries, function arguments, built-in variables (2.5).
- Tables: `[16384, 8192, 4096, 2048][i]`. The index is truncated; out of range is 0.
- Registers: `reg[0xF4]` is the byte the DUT sees now at that register (its own writes
  included). `reg16be[a]`, `reg16le[a]` read two bytes.
- Strings: `'N'` (no escapes), where a template or an answer takes text (5.1). A `let`, a constant
  or a function may hold a string, `c ? 'a' : 'b'` picks one, and `==` and `!=` compare them; any
  other operator on a string is an error.

### 2.2 Operators

By precedence, high to low:

| Operators | Notes |
|---|---|
| `( )` `[ ]` calls | |
| `!` `~` unary `-` | `~` on the integer value |
| `**` | power, right associative |
| `*` `/` `%` | `/` is a float division; use `idiv(a, b)` for C integer division |
| `+` `-` | |
| `<<` `>>` | on integers: `>>` is an arithmetic shift (rounds toward minus infinity, like C on a signed int) |
| `&` `^` `\|` | on integers |
| `<` `<=` `>` `>=` `==` `!=` | 1 or 0 |
| `&&` `\|\|` | short circuit |
| `c ? a : b` | |

Within a row the C order holds: `&` before `^` before `|`, `<` `<=` `>` `>=` before `==` `!=`, `&&`
before `||`. Unlike C, comparisons bind looser than the bitwise operators, so `reg[0x6B] & 0x80 == 0x80`
means `(reg[0x6B] & 0x80) == 0x80`. Unary minus binds tighter than `**`: `-2 ** 2` is 4.

Integer operators work on the value truncated to a 64-bit integer. `%` is the float remainder (the
sign of the dividend, like C `fmod`). A shift by 64 or more gives 0 (`>>` of a negative value: -1);
a negative shift count shifts nothing.

### 2.3 Built-in functions

| Function | Result |
|---|---|
| `abs min max clamp(x, lo, hi)` | |
| `round floor ceil trunc` | `round` rounds half away from zero |
| `sqrt exp log log10 pow sin cos tan asin acos atan atan2` | radians |
| `rad(d) deg(r)` | |
| `idiv(a, b) imod(a, b)` | C integer division and remainder (round toward zero) |
| `u8 s8 u16 s16 u24 s24 u32 s32(x)` | wrap to that integer type, as a C cast does |
| `sat_u8 sat_s8 sat_u16 sat_s16 sat_u24 sat_s24 sat_u32 sat_s32(x)` | round, then saturate |
| `bit(x, n)` | bit n of x, 0 or 1 |
| `bits(x, hi, lo)` | the field x[hi:lo] |
| `byte(x, n)` | byte n of x, 0 = least significant |
| `crc8(poly, init, b...)` | CRC-8, MSB first, no reflection, no final XOR (Sensirion: `crc8(0x31, 0xFF, hi, lo)`) |
| `crc16(poly, init, reflect, xorout, b...)` | CRC-16 (Modbus: `crc16(0x8005, 0xFFFF, 1, 0, ...)`); `reflect: 1` runs the register LSB first with the reflected polynomial, `init` as given |
| `sum8(b...)` `xor8(b...)` | 8-bit sum and XOR |
| `solve(f, target, lo, hi)` | see 2.4 |
| `random(seed)` | a stable pseudo random number in [0, 1) (a hash of the seed) |
| `hhmmss ddmmyy nmea_lat nmea_lon` | text, see 5.3 |

`min` and `max` take 2 to 16 arguments. The pod computes `sin`, `exp`, `log`, `pow` and the others
with its own fixed algorithms, so a definition gives the same bytes on the pod, in the browser
preview and on the server.

### 2.4 Functions and `solve`

A function has `args`, an optional `let` list (evaluated in order) and a `return` expression.
Functions may call earlier functions, never themselves.

`solve(f, target, lo, hi)` finds the integer `x` in `[lo, hi]` for which `f(x)` is closest to
`target`, by bisection. `f` is the name of a one-argument function that rises or falls
monotonically over the range (the direction comes from `f(lo)` and `f(hi)`). The search stops at an
exact hit; otherwise the result is the closest of the values it tried. A value of `f` that is not a
number (a division by zero) counts as past the far end. This lets you write the datasheet's **decode** formula (the code your
driver runs, raw value to °C) and let the pod find the raw value, instead of inverting the formula
yourself. At most 64 iterations; the pod reports a function that is not monotonic.

Functions with more arguments are solved over the argument marked `_`: `solve(press_pa(_, adc_t),
pressure_pa, 0, 0xFFFFF)`. The other arguments are computed once, before the search. `hi - lo`
is at most 4e18.

Functions may use params, constants, states and lets besides their arguments. A function that
reads a `let` can only be called after that let is set (from a later let, or from registers and
messages). Fragments (5.1) may call functions and earlier fragments; functions cannot call
fragments.

### 2.5 Built-in variables

| Name | Value |
|---|---|
| `t` | seconds since the sensor started (float) |
| `utc` | `utc_start` param if the definition has one, plus `t`; seconds since 1970 |
| `written`, `value` | address and value of the last DUT write (in `on_write`) |
| `count` | how many times this answer or message was sent before |
| `i` | the repeat index of a message (5) |
| `dt` | seconds since the previous state update (5.3) |
| `fifo_level` | frames waiting in the FIFO (3.5) |
| `input` | analog with `excitation: adc`: the voltage the pod measures (6) |

`written` and `value` read -1 when the pod only knows that the DUT wrote, not where.

## 3. Registers (I2C and SPI)

I2C and SPI sensors share the register model: an image of up to 256 bytes that the DUT reads and
writes through a register pointer.

```yaml
registers:
  - { at: 0xD0, u8: 0x58, name: ID }
  - { at: 0x88, u16le: "T1" }
  - { at: 0xF7, bytes: ["byte(adc_p, 2)", "byte(adc_p, 1)", "(adc_p & 0x0F) << 4"] }
  - { at: 0x3B, s16be: "asleep ? 0 : ax * accel_lsb", name: ACCEL_XOUT }
  - { at: 0xF4, u8: 0x00, name: CTRL_MEAS }
```

- Field types: `u8 s8 u16be u16le s16be s16le u24be u24le s24be s24le u32be u32le s32be s32le`
  (rounded half away from zero, then saturated to the type), `bytes` (a list of expressions, each
  rounded and saturated to 0..255; use `u8()` or `& 0xFF` to wrap instead), `text` (a fixed ASCII
  string).
- **When a register is loaded.** A register whose expression is a constant is loaded once, at power
  on and on a reset; after that it holds what the DUT writes (configuration). A register whose
  expression uses a param, `reg[]`, `t` or a `let` that does, is rebuilt on every change: a new
  value, a stimulus step, a DUT write. Override with `reload: power_on` or `reload: always`.
- Entries are applied in order, `registers` then `commands`; where two overlap, the later one
  wins, also when only one of them is rebuilt. An entry that runs past the end of the image wraps
  to its start (after `wrap`).
- Bytes no entry covers read `0x00`, or `fill:` if set at the top level.
- `writable: false` keeps the DUT from changing a register (the pod restores it).

### 3.1 Commands

Some sensors are commanded, not addressed: the DUT writes a command byte and reads the answer
(Sensirion). The command byte becomes the register pointer, so an answer is a register entry at
the command's address:

```yaml
commands:
  - command: 0xFD           # measure, high repeatability
    answer: ["byte(st, 1)", "byte(st, 0)", "crc8(0x31, 0xFF, byte(st, 1), byte(st, 0))",
             "byte(srh, 1)", "byte(srh, 0)", "crc8(0x31, 0xFF, byte(srh, 1), byte(srh, 0))"]
    busy_us: 8300           # reads are not acknowledged until the measurement is done
```

`commands` entries are `registers` entries with a `busy_us` (2-byte commands: `command: 0x2400`,
needs `register_address_bits: 16`).

### 3.2 Conversion

```yaml
conversion:
  trigger: 0xF4             # a DUT write to this register starts a conversion
  time_us: 5500
  busy: { at: 0xF3, mask: 0x08 }   # OR-ed in while converting, or:
  while_busy: flag          # flag (default) | nack (reads not acknowledged) | stretch (I2C clock stretching)
```

### 3.3 Reacting to DUT writes

```yaml
on_write:
  - when: "written == 0xE0 && value == 0xB6"    # BME280 soft reset
    do: reset                                    # reload the power-on image
  - when: "reg[0x6B] & 0x80"                     # MPU-6050 DEVICE_RESET
    do: reset
  - when: "written == 0x38"
    do: { set: { at: 0x3A, u8: 0 } }             # write to one register clears another
```

Rules run after every DUT write, in order, with `reg[]` showing the write. `do` is `reset` or a
`set` (one register entry, written after the rebuild). The first rule that resets wins. Register
expressions that read `reg[]` are rebuilt after every write anyway, so most sensors only need
`on_write` for resets.

### 3.4 Data-ready pin

```yaml
data_ready:
  active: high              # high | low
  mode: pulse               # pulse | level
  pulse_us: 50
  on: conversion            # conversion (end of each conversion) | sample (each FIFO frame or stimulus step) | rate
  rate_hz: "1000 / (1 + reg[0x19])"   # with on: rate
  enabled: "reg[0x38] & 1"  # optional condition (the DUT's interrupt enable)
  clear_on_read: 0x3A       # level mode: reading this register releases the pin
```

The pin is assigned when the sensor is enabled (wiring name `drdy` or any LA channel).

### 3.5 FIFO

```yaml
fifo:
  at: 0x74                  # reads of this register pop the FIFO; the pointer does not move
  depth: 64                 # frames
  frame: ["byte(sat_s16(ax * accel_lsb), 1)", "byte(sat_s16(ax * accel_lsb), 0)", ...]
  rate_hz: "1000 / (1 + reg[0x19])"
  enabled: "reg[0x23] & 0x08"
  overflow: drop_oldest     # drop_oldest | drop_newest
```

Use `fifo_level` in a register entry to report the fill level (`{ at: 0x72, u16be: "fifo_level * 6" }`).

## 4. Bus settings

```yaml
i2c:
  address: 0x76
  alt_addresses: [0x77]     # selectable when the sensor is enabled
  register_address_bits: 8  # 8 | 16
  page: 0x0000              # 16-bit addresses: the 256-byte window the image covers
  auto_increment: true
  wrap: 0xFF                # the pointer wraps after this address
```

```yaml
spi:
  mode: 0                   # 0..3
  read_bit: { bit: 7, value: 1 }       # first byte: this bit selects a read
  increment_bit: { bit: 6, value: 1 }  # optional: this bit enables auto increment (otherwise always on)
  address_mask: 0x3F                   # the bits of the first byte that are the address
  dummy_bytes: 0            # bytes between the address and the read data
```

## 5. UART sensors

```yaml
kind: uart
uart: { baud: 9600, format: 8N1 }
messages:                   # sent on their own
  - every_ms: "1000 / rate_hz"
    text: "$GPRMC,{hhmmss(utc)},{fix ? 'A' : 'V'},{nmea_lat(lat_deg)},...,A"
    end: nmea               # nmea: append *hh\r\n | crlf | lf | none
  - every_ms: "1000 / rate_hz"
    repeat: "ceil(sats / 4)"           # one message per value of i, from 0
    text: "$GPGSV,{ceil(sats / 4)},{i + 1},{sats:02d},..."
    end: nmea
requests:                   # answers to what the DUT sends
  - match: [0xFF, 0x01, 0x86, "*", "*", "*", "*", "*", "*"]   # "*" matches any byte
    answer: [0xFF, 0x86, "byte(co2_ppm, 1)", "byte(co2_ppm, 0)", 0, 0, 0, 0, "checksum_mhz19"]
    delay_ms: 10
  - match_line: "AT+READ?"  # a line, without its CR/LF
    text: "+READ: {co2_ppm:d}"
    end: crlf
```

Messages with the same `every_ms` go out together, in file order (one epoch, like a GPS fix).
`enabled:` (an expression) skips a message; `repeat: n` sends it n times with `i` = 0 .. n-1 (at
most 64). `every_ms` is truncated to whole milliseconds (at least 1); each message keeps its own
schedule, starting at t = 0, and `t` in an epoch is the time it was due. `end` defaults to `none`;
the `nmea` checksum covers the characters after the leading `$` or `!`.

### 5.1 Templates

`text` is literal text with `{expression}` or `{expression:format}` parts; `{{` and `}}` are
literal braces. Formats: `d`, `x`, `X`, `f`, with the flags `-` `+` `0`, a width (at most 64) and a
precision (at most 15) as in C printf (`{lat:09.4f}`, `{sats:02d}`). `d`, `x` and `X` truncate to
an integer; `f` rounds half away from zero (default precision 6). Without a format, a whole number
prints as an integer and others with up to 6 decimals, trailing zeros dropped. Numbers of 1e18 and
more print as `d.dddddde+NNN`.

The format is what follows the last `:` outside brackets and quotes, when it reads as a format and
does not start with a space, so write a ternary's `:` with a space after it: `{have ? sats : 0:02d}`.
An expression may give a string literal: `{lat >= 0 ? 'N' : 'S'}`; a string ignores the number
format, so `{have ? hdop : '':.2f}` prints an empty field without a fix.

`fragments:` are named pieces of text with optional `args`, used like functions inside a template:

```yaml
fragments:
  sat:
    args: [j]
    text: ",{prn(j):02d},{elev(j):02d}"
messages:
  - every_ms: 1000
    text: "$GPGSV,1,1,04{sat(0)}{sat(1)}{sat(2)}{sat(3)}"
```

### 5.2 Byte answers

`answer` is a list of byte expressions, as in `bytes` (3). `match` lists bytes or `"*"` (at most
64) and matches the last bytes received; `match_line` matches a whole line without its CR/LF. A
request must arrive within 100 ms between bytes, else matching starts over. The first request that
matches answers, after `delay_ms`.

### 5.3 State

Some sensors integrate over time: a GPS position moves along its course at its speed.

```yaml
state:
  - name: lat
    init: lat_deg            # at start
    update: "lat + speed_kmh / 3.6 * dt * cos(rad(course_deg)) / 111320"
    follow: lat_deg          # optional: a new value of this param resets the state to init
```

State is updated before each epoch (UART) or each rebuild (I2C, SPI), in order: a later state sees
the new value of an earlier one. `dt` is the time since the previous update in seconds (0 at the
start). State expressions use params, constants, functions, states and built-in variables, not
lets. State works in every kind.

Helpers for common formats: `hhmmss(utc)` (`123456.00`, hundredths of a second), `ddmmyy(utc)`,
`nmea_lat(deg)` (`4807.03800,N`), `nmea_lon(deg)` (`01131.00000,E`), minutes with 5 decimals. They
are ordinary built-ins and may be used anywhere.

## 6. Analog sensors

```yaml
kind: analog
output: voltage             # voltage (DAC) | current (4-20 mA output, in mA)
value: "3.3 * r_ntc / (r_ntc + 10000)"
let:
  - r_ntc: "10000 * exp(3950 * (1 / (temperature_c + 273.15) - 1 / 298.15))"
range: { min: 0, max: 3.3 } # the output is clamped to this range
excitation: none            # none | adc: the value may use `input`, the voltage the pod measures
```

A value that is not a number gives `range.min`. An analog sensor only maps a value to an output. It uses the pod's existing outputs: a set value is
a DC output, a stimulus becomes a waveform, a recording stays a recording.

## 7. Stimulus

```yaml
format: 1
kind: stimulus
name: hot_afternoon
repeat: false               # loop the whole stimulus
signals:
  temperature_c:            # a param; with several sensors: env.temperature_c
    - { shape: hold, value: 22, duration_s: 5 }
    - { shape: ramp, end: 35, duration_s: 30 }          # start: the previous end, or `start:`
    - { shape: sine, offset: 35, amplitude: 1.5, period_s: 10, duration_s: 40 }
    - { shape: noise, mean: 35, sigma: 0.2, duration_s: 20, seed: 7 }
    - { shape: step, value: 25 }                         # instant, duration 0
  pressure_pa:
    - { shape: data, points: [[0, 101325], [10, 100900], [60, 99000]], interpolate: linear }
faults:
  - { at_s: 45, duration_s: 5, type: stuck }
  - { at_s: 70, duration_s: 2, type: no_response }
```

Segment shapes:

| Shape | Fields |
|---|---|
| `hold` | `value`, `duration_s` |
| `step` | `value` (duration 0) |
| `ramp` | `start` (optional), `end`, `duration_s` |
| `sine` | `offset`, `amplitude`, `period_s`, `phase_deg`, `duration_s` |
| `saw` | `low`, `high`, `period_s`, `duration_s` |
| `pulse` | `low`, `high`, `period_s`, `duty` (0..1), `duration_s` |
| `noise` | `mean` (optional: the previous value), `sigma`, `seed`, `duration_s` |
| `data` | `points` ([seconds, value] pairs) or `file` (CSV `t,value`), `interpolate`: `linear` or `step` |
| `repeat` | `count`, `segments` (a nested list) |

Signals run side by side; each holds its last value after its last segment. Values are applied at
up to 100 updates per second (`rate_hz` at the top level, default 50), and clamped to the param's
range. A segment without `start` (or `mean`) starts from the value the previous one ended with:
`sine`, `saw` and `pulse` end with their value at their end time, `noise` with its mean. Noise is a
function of `seed` and the sample number in the segment (`rate_hz` samples per second), so a replay
is the same. `data` points start at 0 s or later and never go back in time; before the first point
the signal has the first value. The pod takes `points` only: `tools/yaml2json.py` reads a `file`
and inlines it. `repeat` needs `count` of 1 or more and nests at most 4 deep; `repeat: true` at the
top level loops signals and faults with the longest signal's duration.

Limits per stimulus: 16 signals, 256 segments (repeats count their body once), 1024 points per
`data` segment, 32 faults, 8 KB compiled.

## 8. Faults

Faults are a list in a stimulus file, or applied immediately through the API. Each has `type`, and
optionally `at_s`, `duration_s` (default: until cleared), `sensor` (with several sensors) and
`param`. `stuck` without `param` freezes the whole sensor; with one, that param. `offset` and
`gain` need `param` and `value`, `corrupt` needs `at` and `mask`, `drop` a `value`, `baud_error` a
`value`; `garbage` takes `rate_hz` (default 100).

| Type | Kinds | Effect |
|---|---|---|
| `stuck` | all | readings freeze at their current (or `value`) value |
| `offset` | all | `param` reads `value` higher |
| `gain` | all | `param` reads times `value` |
| `no_response` | i2c, spi, uart | I2C: the address is not acknowledged; SPI: MISO stays released; UART: nothing is sent |
| `corrupt` | i2c, spi | register `at` reads XOR `mask` (a broken CRC: `at` = the CRC byte) |
| `bus_stuck` | i2c | SDA held low for `duration_s` (at most 1 s) |
| `garbage` | uart | random bytes at `rate_hz` instead of the messages |
| `drop` | uart | each message is dropped with probability `value` (0..1) |
| `baud_error` | uart | the baud rate is off by `value` percent |
| `open` | analog | output at `range.min` (or `value`) |
| `short` | analog | output at `range.max` (or `value`) |

## 9. Several sensors at once

A pod runs up to 4 I2C sensors (on one bus or on several), 1 SPI sensor, 1 UART sensor and analog
sensors on its analog outputs, at the same time. Each enabled sensor gets an instance name (default:
its `name`), which stimulus signals and faults use as a prefix (`env.temperature_c`).

## 10. Errors

Every error names the file location as a path (`registers[3].s16be`) and, for expressions, the
character offset: `registers[3].s16be: unknown name 'acel_lsb' at 12`. The pod also runs a new
definition once at its defaults, so an evaluation error (a string in arithmetic, a `solve` that is
not monotonic, the step limit) is reported with its path when the definition is loaded.
