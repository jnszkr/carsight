# carsight

Read live data from our cars over OBD-II, store it, and use AI to analyse it: spot anomalies, track engine health over time, and explain fault codes in plain language.

## Goals

1. **Log** live engine data from the OBD-II port to local storage while driving.
2. **Store** every drive as a structured session (car, start/end time, samples, DTCs).
3. **Analyse** sessions with an LLM (Claude): summarise a drive, compare it against the car's baseline, flag anything unusual, and explain trouble codes.
4. **Track trends** across many drives (e.g. fuel trims drifting, coolant warming up slower, rising idle RPM).

## Hardware

| Item | Notes |
|---|---|
| **Vgate vLinker FS USB** | ELM327-compatible, FTDI USB serial, auto HS-CAN / MS-CAN switching |
| Laptop | macOS (FTDI driver built in), shows up as `/dev/tty.usbserial-*` |

### Adapter: Vgate vLinker FS USB

Sold as "Made for FORScan", but it is a general OBD-II adapter that speaks the ELM327 command set, which is all carsight uses.

| | |
|---|---|
| USB interface | FTDI virtual COM port (VCP) bridge. macOS ships the FTDI driver; on Windows it may need the [FTDI VCP driver](https://ftdichip.com/drivers/vcp-drivers/). |
| Serial speed | Opens at **115200 baud** (FORScan's documented setting). The link can go up to **3 Mbaud** (FORScan's "auto-increase"), but each reading is limited by how fast the ECU answers, so carsight stays at 115200. `-baud 0` (default) tries 115200 first. |
| Identification | Reports itself as e.g. `vLinker FS v2.2.58 MIC332X v2.0 (ELM327 v2.2)`. The logger prints the ELM327 version line from `ATZ` on connect. |
| Buses | Automatic electronic HS-CAN / MS-CAN switching (no manual toggle switch). Standard engine data is on HS-CAN; MS-CAN only matters for Ford/Volvo body modules. |
| Power | Safe on 12 V and 24 V systems. "Battery saver": goes into standby on its own and wakes when used, so it can stay plugged in. |
| Buffers | 8192-byte serial buffer; requests up to 4128 bytes (long multi-frame messages). |
| FEPS | Supplies the 18 V Ford programming voltage for ECU firmware flashing. carsight never uses it. |

**LEDs:** plugged into the laptop only, the red LED flashes once and the blue LED stays on. Plugged into the car as well, blue and green stay on.

**Connecting:**

1. Plug the adapter into the laptop, then into the car's OBD-II port (under the dashboard, driver's side).
2. Turn the ignition **on** (engine running for a drive log; engine off with ignition on for `clear-dtcs`). On push-button cars without a key, press Start twice without the brake for ignition-on.
3. Run `./carsight-logger scan`. If no adapter is found, check that `ls /dev/tty.usbserial-* /dev/cu.usbserial-*` lists it, try another USB port, or pass the path with `-port`.

**Firmware:** updates and the full user manual are on Vgate's [download centre](https://www.vgatemall.com/downloadcenter) (the updater is Windows-only). Vgate recommends firmware **v2.2.78 or newer** for cars from 2005 and earlier; neither of our cars is that old, but update if a K-line connection to the Citroën is unreliable.

**FORScan:** the adapter's bundled guide is for FORScan (Windows, Ford/Mazda/Lincoln): Settings → Connection, type FTDI, baud 115.2 Kbps, auto-increase on. carsight doesn't need it. Only one program can hold the serial port at a time, so close other OBD apps before logging.

## Vehicles

| Car | Year | Notes |
|---|---|---|
| Volvo V70 | 2011 | P3 platform (Ford-related). Rich standard PID set. Extra modules on MS-CAN, which the vLinker FS can reach. |
| Citroën C4 | 2006 | PSA. Fewer standard PIDs. Generic engine data only; PSA-specific modules are out of scope for v1. |

## Architecture

```
vLinker FS (USB serial)
      │  ELM327 AT commands
      ▼
cmd/carsight-logger   ── polls selected PIDs, reads DTCs
      │
      ▼
storage (SQLite first, PostgreSQL later)
  sessions · samples · dtcs · vehicles
      │
      ▼
cmd/carsight-analyze  ── builds a compact drive summary + stats
      │                  and sends it to Claude for analysis
      ▼
report (Markdown per drive / per car)
```

### Components

- **`internal/elm327`**: serial connection, AT init (`ATZ`, `ATE0`, `ATL0`, `ATS0`, `ATSP0`), request/response parsing, protocol detection.
- **`internal/obd`**: PID definitions and decoding (mode 01 live data, mode 03/07 DTCs, mode 09 VIN), and supported-PID discovery.
- **`internal/store`**: session and sample persistence.
- **`internal/analyze`**: per-drive statistics (min/max/mean, warm-up time, idle stability, fuel trim behaviour), baseline comparison, and LLM prompt building.
- **`cmd/carsight-logger`**: CLI that connects, identifies the car by VIN, and logs a session until stopped.
- **`cmd/carsight-analyze`**: CLI that analyses one session or a date range and writes a report.

## Usage

Requires Go 1.27+. No cgo: SQLite is the pure-Go `modernc.org/sqlite`.

```bash
go build ./cmd/...
```

### Logging

Plug in the vLinker, turn the ignition on, then:

```bash
./carsight-logger scan                                   # protocol, VIN, supported PIDs, DTCs, readiness
./carsight-logger -name "Volvo V70" -notes "cold start, commute"   # log until Ctrl-C
./carsight-logger clear-dtcs                             # asks for confirmation
```

`-port auto` (the default) picks the first `/dev/tty.usbserial-*`; the baud rate is auto-detected. Fast PIDs (RPM, speed, load, throttle, MAF/MAP, STFT, timing, O2) are polled every cycle and slow ones (temperatures, LTFT, voltage) every `-slow-every` cycles. On CAN, each request tells the adapter to expect a single answer so it returns without waiting for its timeout. VIN, DTCs (at start and end) and monitor readiness are stored once per session.

No car handy? `-port sim:volvo` or `-port sim:citroen` runs against a simulated adapter (CAN / KWP2000, with a warming engine and a stop-and-go drive). `-sim-speedup 30` compresses time, which also blurs PID-to-PID timing, so don't read too much into simulated idle figures.

### Analysis

```bash
./carsight-analyze -list                    # vehicles and sessions
./carsight-analyze                          # report on the latest drive -> reports/<VIN>-session-<id>.md
./carsight-analyze -session 12 -ai          # include Claude's analysis
./carsight-analyze -trend -since 2026-09-01 -ai   # trend report per car -> reports/<VIN>-trends.md
./carsight-analyze -session 12 -json        # the exact summary that would be sent to Claude
```

A drive report has rule-based findings (overheating, slow warm-up, unstable idle, fuel trims beyond ±10%, inactive O2 sensor, charging voltage, DTCs, MIL), warm-engine figures, a comparison against the car's last `-baseline` drives (z-scores, flagged at |z| ≥ 2 once there are 3+ baseline drives), DTCs and readiness. Drives shorter than `-min-duration` (5 min) are left out of baselines and trends.

`-ai` sends the compact summary (never raw samples) to Claude (`claude-opus-5-5` by default, `-model` to change) and adds its write-up to the report. It needs an Anthropic API key from the [Claude Console](https://console.anthropic.com/settings/keys), exported as `ANTHROPIC_API_KEY` (API usage is billed separately from a Claude subscription; logging in to Claude Code doesn't provide a key). Requests opt into server-side refusal fallback, so a safety-classifier decline is re-served by another model instead of failing.

### Layout

| Package | Purpose |
|---|---|
| `internal/elm327` | Serial protocol: AT init, prompt handling, error mapping, ISO-TP multi-frame reassembly |
| `internal/obd` | PID definitions and decoding, supported-PID bitmaps, DTCs (CAN and K-line formats), VIN, readiness |
| `internal/elmsim` | Simulated ELM327 + car for development and tests |
| `internal/store` | SQLite schema and queries |
| `internal/analyze` | Drive summary, events, baseline comparison, trends, Markdown reports, Claude client |

## Data to log (v1 PID set)

Keep the set small so the sample rate stays high (target ≥ 5 Hz per PID group):

- Engine RPM, vehicle speed, engine load
- Coolant temperature, intake air temperature
- MAF and/or MAP, throttle position
- Short- and long-term fuel trims (bank 1)
- Timing advance
- O2 sensor voltages (if supported)
- Control module voltage

Plus once per session: VIN, stored/pending DTCs, and the monitor readiness status.

## AI analysis

Raw samples are **not** sent to the model. The analyser first reduces a drive to a compact summary (stats, warm-up curve, notable events, DTCs, deltas vs. that car's baseline) and sends that to Claude with a fixed prompt asking for:

- a short health summary of the drive
- anomalies, ranked by how much they matter
- likely causes and what to check next
- plain-language explanations of any DTCs

Good analysis depends on baselines: record several consistent drives per car (same route, cold start to fully warm) before trusting the anomaly output.

## Roadmap

Implemented and tested against the built-in simulator; not yet verified on the real cars:

- [x] ELM327 serial connection + init against the vLinker FS
- [x] Supported-PID discovery for both cars
- [x] Logger CLI writing to SQLite
- [x] Read and clear DTCs, read VIN
- [x] Per-drive stats + Markdown report
- [x] Claude analysis of a single drive
- [x] Baseline per car + trend comparison across drives

Next:

- [ ] Verify on the Volvo (CAN) and Citroën (K-line); tune thresholds from real baselines
- [ ] Multi-PID requests on CAN to raise the sample rate
- [ ] Optional: Bluetooth adapter / phone logging, PostgreSQL, web dashboard
- [ ] Optional: Volvo MS-CAN module data

## Out of scope (for now)

- Writing to ECUs, coding, or clearing anything other than DTCs
- Manufacturer-specific diagnostics (VIDA/DiCE for Volvo, Diagbox for PSA)
- Real-time alerts while driving

## Safety

The logger is read-only apart from an explicit "clear DTCs" command. Don't operate the laptop while driving: start logging before setting off, or have a passenger handle it.
