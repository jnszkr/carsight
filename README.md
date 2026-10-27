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

## Vehicles

| Car | Year | Notes |
|---|---|---|
| Volvo V70 | 2011 | P3 platform (Ford-related). Rich standard PID set. Extra modules on MS-CAN, which the vLinker FS can reach. |
| Citroën C4 | 2006 | PSA. Fewer standard PIDs. Generic engine data only; PSA-specific modules are out of scope for v1. |

## Architecture (planned)

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

- [ ] ELM327 serial connection + init against the vLinker FS
- [ ] Supported-PID discovery for both cars
- [ ] Logger CLI writing to SQLite
- [ ] Read and clear DTCs, read VIN
- [ ] Per-drive stats + Markdown report
- [ ] Claude analysis of a single drive
- [ ] Baseline per car + trend comparison across drives
- [ ] Optional: Bluetooth adapter / phone logging, PostgreSQL, web dashboard
- [ ] Optional: Volvo MS-CAN module data

## Out of scope (for now)

- Writing to ECUs, coding, or clearing anything other than DTCs
- Manufacturer-specific diagnostics (VIDA/DiCE for Volvo, Diagbox for PSA)
- Real-time alerts while driving

## Safety

The logger is read-only apart from an explicit "clear DTCs" command. Don't operate the laptop while driving: start logging before setting off, or have a passenger handle it.
