# prog_devices.json

The device table of the pin programmers (`BenchPod.flash(target="avr-updi" | "pic" | "efm8" |
"attiny-tpi" | "xmega")`, the pod's `prog_flash` command). The pod holds no device tables: it reads
the chip's ID bytes and the host passes the geometry. This file is that table, plain JSON, one chip
per line, so other clients (the CLI) can copy it unchanged. The SDK reads it in
`embeddedci/benchpod/prog.py`.

**It is generated.** The source is `prog-devices/` in the embeddedci-python repository: one YAML file
per chip, grouped by protocol and family, with typed fields (schema and workflow in
`prog-devices/README.md`). `scripts/gen_prog_devices.py` writes this file, the CLI's copy and the web
app's detect table; CI fails when any of them differs from the source. Do not edit them by hand.

## Schema (`"schema": 2`)

```
{ "schema": 2, "about": "...",
  "families": { "<proto>": { "target": "...", "title": "...", "ignore": [...], "devices": [...],
                             "entry_styles": [...] } } }
```

`entry_styles` (`pic` only): the distinct ways the table's PICs are entered, the plain 6-bit set first,
each `{"prog": {...}, "flags": n}`: what a host tries, in turn, when no chip is named (a PIC answers
only to its own command set).

`<proto>` is the pod's `proto`: `updi`, `pic`, `c2`, `tpi`, `pdi`. `target` is the
`flash(target=...)` name of the family (`avr-updi`, `pic`, `efm8`, `attiny-tpi`, `xmega`); the
protocol names are accepted as aliases too.

`ignore` lists image regions to leave out without an error: `{"hex": address, "size": bytes,
"name": text}` (the AVR lock bits and signature row that an avr-gcc `.elf` carries).

A device:

| Key | Meaning |
|---|---|
| `name` | The chip, or the family of chips that share the ID and memories. Matched ignoring case, spaces and dashes. |
| `aliases` | (optional) other names for the same ID, for example `ATxmega16A4` next to `ATxmega16A4U`. |
| `id` | Hex bytes, in the order of the pod's `status.id`. `??` is a byte that varies (a revision). The ID is matched as a prefix of what the pod reports: the pod's `expect_id` is `id` up to its first `??`. |
| `family` | Free text, for messages. |
| `mem` | The chip's memories, keys `flash`, `eeprom`, `config` (AVR fuses, PIC configuration words, C2 lock byte) and `user` (AVR user row, PIC user ID); each omitted when the chip has none or the pod cannot write it. |
| `prog` | The chip's protocol options as named fields, copied into every `start` (and PIC `info`) request of a pod with the `prog_named` feature (firmware 4.0.6 and later): `updi` `flash_base`; `pic` `cmd_set` (`6bit`/`8bit`), `pc` (`byte`), `ee_erase`, `cp_high`, `q10`, `id_split`, `key_flip`, `cp_bit`, `cp_addr` (the byte address, as `addr` counts, of the configuration word that holds the code-protect bit); `tpi` `write_words`, `page_erase`; `c2` `fpdat`, `no_setup`. Meaning of each: the pod's `prog_flash` command in the firmware API.md. Omitted when the chip needs none. |
| `legacy` | The same options as the one packed integer `flags` (and PIC `cp_addr`) that firmware before 4.0.6 reads: sent instead of `prog` to such a pod, and dropped together with that path. Generated from `prog`; never edited. |
| `vmin_mv`, `vmax_mv` | The supply range the chip can be programmed in, in millivolts, at the pod's I/O voltage (1800 or 3300). A chip whose `vmin_mv` is above the pod's voltage is refused with `vnote` as the reason. |
| `erase_vmin_mv` | (optional) the supply the chip erase needs: below it the erase can be ignored, so a `chip` erase is refused. |
| `vnote` | (optional) the voltage remark of the datasheet. |
| `notes` | (optional) other remarks. |

A memory (`mem.<name>`):

| Key | Meaning |
|---|---|
| `size` | Bytes. |
| `page` | The write page or row in bytes: the `page` of `start`. |
| `addr` | The `addr` of `start` for the memory's first byte (the pod's address space: AVR memories start at 0; PIC addresses are the spec's byte address, 2 per word). |
| `hex` | The address of the memory's first byte in a `.hex` / `.elf` image (avr-gcc: flash 0, EEPROM 0x810000, fuses 0x820000, user row 0x850000; PIC: see below). |
| `align` | (optional) region start and end unit in bytes; default: `page` for flash, 1 otherwise. A partial unit is padded with 0xFF. |
| `stride` | (optional) hex bytes one byte of this memory takes; default 1. |

To write an image: read the chip's ID with `info`, find the device whose `id` matches, and for every
image run of bytes find the memory whose `hex` range holds it. The pod address of image byte `a`
is `mem.addr + (a - mem.hex) / stride`. Start the regions flash first, `config` last, each as one
`start` with `mem`, `addr`, `len`, `page`, `expect_id`, the chip's `prog` fields (`legacy` for a pod before 4.0.6), and `erase: "chip"` on the
first one only (later ones `none`).

## Sources

Every family's numbers come from the vendor documents below; where a vendor's tools were used to
cross-check, that is said. Nothing here was run on hardware yet.

### AVR UPDI (`updi`): tinyAVR 0/1/2, megaAVR 0, AVR DA/DB/DD

* Reference datasheets: ATtiny1614/1616/1617 DS40002204 (chapters NVMCTRL and UPDI) and AVR128DA
  DS40002183 (the same chapters for NVM version 2); the other chips' datasheets of each family give
  the signature (SIGROW), flash, EEPROM and user row sizes and pages.
* The IDs, sizes and pages were read from avrdude's `avrdude.conf` (`src/avrdude.conf.in`,
  github.com/avrdudes/avrdude), which carries them from the Atmel/Microchip device packs, and
  checked against the datasheets above.
* The flash base in the AVR data space (`prog.flash_base`) follows the firmware: 0x8000 for tinyAVR
  0/1/2, 0x4000 for megaAVR 0, 0x800000 for the NVM version 2 parts (DA, DB, DD).
* Left out: AVR DU, EA, EB, LA, SD and the "S" variants (other NVM versions, not handled by the pod
  firmware yet).
* Voltage: 1.8-5.5 V for the whole range (the datasheets' operating range; NVM writes are specified
  over it).

### AVR TPI (`tpi`): ATtiny4/5/9/10/20/40/102/104

* ATtiny4/5/9/10 DS40002060, ATtiny20 8235, ATtiny40 8263, ATtiny102/104 42505: signatures, flash
  size and page, the TPI programming chapters (memory map: lock bits 0x3F00, configuration 0x3F40,
  signature 0x3FC0, flash 0x4000), the number of words per flash write (ATtiny20: 2, ATtiny40: 4)
  and the page erase of the ATtiny102/104.
* IDs and sizes cross-checked with avrdude's `avrdude.conf`.
* **Voltage:** the ATtiny4/5/9/10, 20 and 40 can be programmed at 5 V only (datasheets: "Apply 5V",
  4.75-5.5 V in the programming tables). The pod's I/O is 1.8 or 3.3 V, so these are marked
  `vmin_mv: 4750` and refused with a message. The ATtiny102/104 datasheet gives 1.8-5.5 V in one
  section and 5 V in another: they are allowed and the note says to try it.

### XMEGA PDI (`pdi`)

* XMEGA AU manual 8331 and the ATxmega datasheets (A4U 8387 and the other chips'):
  signatures, flash (application + boot as one space from 0), EEPROM, fuse bytes, user signature row,
  the NVM controller. IDs, sizes and pages cross-checked with avrdude's `avrdude.conf`; parts that
  share an ID are one entry with `aliases`.
* Voltage: 1.8 to 3.6 V as the table says; the pod's 3.3 V is fine, 1.8 V works when BODLEVEL is
  below VCC (datasheet), which the chip's fuses decide.

### PIC (`pic`)

Every value was read from Microchip's memory programming specifications (not from memory); most PDFs
came from third-party mirrors, none was cross-checked against an MPLAB DFP or gputils.

| Family | Specification | Command set (`prog.cmd_set`, `prog.pc`) |
|---|---|---|
| PIC16F150x | DS41573 | 6-bit, DEV in bits 13:5 of the word at 0x8006 |
| PIC16F151x/152x | DS41442 | 6-bit, packed ID |
| PIC16F170x | DS40001683 | 6-bit, separate REVID word (`id` is the whole DEVID word) |
| PIC16F171x | DS40001714 | 6-bit, separate REVID word |
| PIC16F182x, PIC16F1847 | DS41390 (182x), DS41439 (1847) | 6-bit, packed ID, data EEPROM |
| PIC16F193x | DS41397 | 6-bit, packed ID, data EEPROM |
| PIC16F183xx | DS40001738 | 6-bit (not the 8-bit set), separate REVID word |
| PIC16F184xx | DS40001970 | 8-bit, MSb first |
| PIC16F188xx | DS40001753 | 8-bit |
| PIC18F Q10 | DS40001874 | 8-bit, byte PC, Q10 erase and config words |
| PIC18F Q40 | DS40002185 | 8-bit, byte PC |
| PIC18F Q43 | DS40002079 | 8-bit, byte PC |
| PIC18F Q83, Q84 | DS40002137 | 8-bit, byte PC |

The LF parts have their own device IDs and are separate entries. The legacy `flags` bit values are in
`stm32h563/src/prog_pic.h` of benchpod-firmware. `prog.cp_addr` is the byte address of the
configuration word that holds the code-protect bit (the pod reads it to tell a locked chip), `prog.cp_bit`
its bit number; the polarity is 0 = protected on every chip here (`cp_high` otherwise).
Addresses: flash, configuration words and user ID are the spec's absolute word address times 2
(PIC16: word 0x8007 is 0x1000E, user ID 0x10000); PIC18 uses the spec's byte addresses (0x200000 user
ID, 0x300000 configuration, 0x380000 EEPROM, 0x310000 on Q10). `hex` equals `addr` for all of them.

`id` is what the pod reports in `status.id`: the device ID little endian (2 bytes); on the packed 6-bit
chips DEV<8:0> (the DEVID word shifted right by 5) because the revision shares the word.

Known gaps, so that nothing is written wrongly:

* **No data EEPROM for PIC16F18xxx and the PIC16F1xxx that have one** (`notes` says so): where an
  XC8 `.hex` puts it (0x1E000, two hex bytes per EEPROM byte on PIC16F1xxx per DS41439/41390/41397; not
  stated for 184xx/188xx) is not verified, and a wrong layout would be written without any error.
  PIC18 EEPROM (hex address = programming address, one byte each) is included.
* **PIC16F1827 and PIC16F193x write 8 words per row but erase 32:** `chip_erase_only` is set and
  `page` is the write size, so `erase="page"` is refused.
* **PIC18F Q41, PIC16F150x/151x/152x/170x/171x high-endurance flash and calibration words** are not
  covered. PIC18 Q43/Q83/Q84 user ID size is 32 bytes (the spec's range end is inconsistent: 0x20001F
  or 0x20003F).
* The LVP key bit order of the 6-bit chips follows the spec text (LSb first, 33 clocks); the firmware
  review flagged the same, and it is the first thing to check on a first 6-bit chip.
* `erase_vmin_mv`: bulk erase needs the supply above the chip's brown-out level (2.7 V on the 6-bit
  chips and the F18 specs, about 1.9 V on Q10 and Q40, 2.85 V on Q43/Q83/Q84).

### Silicon Labs C2 (`c2`)

From the EFM8 reference manuals (BB1, BB2, SB1, SB2, UB1, UB2: Memory Map, Device Identification
Registers, Security Options), read on a Keil-hosted mirror, and the BB1/BB2/UB2 datasheets (flash
table: VPROG 2.2-3.6 V, UB2 2.7-3.6 V). AN127 itself could not be read (silabs.com refused the fetch), so
its device table rows are second-hand.

* Covered: EFM8BB1 (ID 0x30: BB10F8G, F4G, F2G), BB2 (0x32), SB1 (0x25), SB2 (0x16), UB1 (0x32), UB2
  (0x28). Chips that share an ID differ in size, so the SDK asks for `device=` (BB1's three sizes, BB2
  against UB1). The pod's derivative-ID read is not documented in the reference manuals, so the
  derivative ID is not used.
* `flash.size` stops below the flash lock byte (0x1FFF on an 8 KB BB1, 0xFBFF on 64 KB parts): the
  lock byte, the lock page's neighbours and the reserved area are never written, and an image with data there is refused.
  There is no `config` memory.
* Not covered, because their IDs or maps could not be confirmed: EFM8BB3, LB1 (both read 0x34), UB3, and all C8051F parts.
* FPDAT (0xB4, or 0xAD on EFM8UB2) is picked by the pod from the device ID; the table does not set it.
