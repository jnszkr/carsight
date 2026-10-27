package analyze

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jnszkr/carsight/internal/obd"
)

// WriteReport renders a Markdown report for one drive. aiAnalysis, if
// non-empty, is included verbatim as its own section.
func WriteReport(w io.Writer, s Summary, deltas []Delta, aiAnalysis string) error {
	var b strings.Builder
	title := s.Vehicle
	if title == "" {
		title = s.VIN
	}
	fmt.Fprintf(&b, "# Drive %d: %s\n\n", s.SessionID, title)
	fmt.Fprintf(&b, "- **Date:** %s\n", s.Start.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "- **VIN:** %s\n", s.VIN)
	if s.Protocol != "" {
		fmt.Fprintf(&b, "- **Protocol:** %s\n", s.Protocol)
	}
	fmt.Fprintf(&b, "- **Duration:** %s, **distance:** %.1f km\n", s.Duration, s.DistanceKm)
	fmt.Fprintf(&b, "- **Samples:** %d (RPM at %.1f Hz)\n", s.SampleCount, s.RPMRateHz)
	if s.Notes != "" {
		fmt.Fprintf(&b, "- **Notes:** %s\n", s.Notes)
	}
	b.WriteString("\n")

	if aiAnalysis != "" {
		b.WriteString("## Analysis\n\n")
		b.WriteString(strings.TrimSpace(aiAnalysis))
		b.WriteString("\n\n")
	}

	b.WriteString("## Findings\n\n")
	if len(s.Events) == 0 {
		b.WriteString("Nothing flagged by the rule-based checks.\n\n")
	} else {
		for _, e := range s.Events {
			fmt.Fprintf(&b, "- **%s** %s", strings.ToUpper(e.Severity), e.Detail)
			if e.At > 0 || e.Lasted > 0 {
				fmt.Fprintf(&b, " (at %s", e.At)
				if e.Lasted > 0 {
					fmt.Fprintf(&b, ", for %s", e.Lasted)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Warm-up\n\n")
	wu := s.WarmUp
	fmt.Fprintf(&b, "Coolant %.0f °C → max %.0f °C", wu.StartC, wu.MaxC)
	if wu.ToWarm != nil {
		fmt.Fprintf(&b, "; reached %.0f °C after %s", WarmC, *wu.ToWarm)
	} else {
		fmt.Fprintf(&b, "; did not reach %.0f °C", WarmC)
	}
	if wu.ColdStart {
		b.WriteString(" (cold start)")
	}
	b.WriteString(".\n\n")

	if s.Idle != nil || s.FuelTrim != nil || s.VoltageRun != nil {
		b.WriteString("## Key figures (warm engine)\n\n| | Mean | Std dev | Range |\n|---|---|---|---|\n")
		row := func(name string, st Stats, unit string) {
			fmt.Fprintf(&b, "| %s | %.2f %s | %.2f | %.2f – %.2f |\n", name, st.Mean, unit, st.Std, st.Min, st.Max)
		}
		if s.Idle != nil {
			row("Idle RPM", s.Idle.RPM, "rpm")
			if s.Idle.MAF != nil {
				row("Idle MAF", *s.Idle.MAF, "g/s")
			}
			if s.Idle.Timing != nil {
				row("Idle timing advance", *s.Idle.Timing, "°")
			}
		}
		if s.FuelTrim != nil {
			row("STFT B1", s.FuelTrim.STFT, "%")
			row("LTFT B1", s.FuelTrim.LTFT, "%")
		}
		if s.O2B1S1Warm != nil {
			row("O2 B1S1", *s.O2B1S1Warm, "V")
		}
		if s.VoltageRun != nil {
			row("Module voltage (running)", *s.VoltageRun, "V")
		}
		if s.FuelTrim != nil {
			fmt.Fprintf(&b, "\nTotal fuel trim: %+.1f%% at idle, %+.1f%% cruising.\n", s.FuelTrim.TotalIdle, s.FuelTrim.TotalCruise)
		}
		b.WriteString("\n")
	}

	if len(deltas) > 0 {
		b.WriteString("## Compared with baseline\n\n| Metric | This drive | Baseline | z | |\n|---|---|---|---|---|\n")
		for _, d := range deltas {
			flag := ""
			if d.Flag {
				flag = "⚠"
			}
			fmt.Fprintf(&b, "| %s | %.2f | %.2f ± %.2f (n=%d) | %+.1f | %s |\n", d.Metric, d.Value, d.Mean, d.Std, d.N, d.Z, flag)
		}
		if deltas[0].N < MinBaseline {
			fmt.Fprintf(&b, "\nBaseline has fewer than %d drives; deviations are not flagged yet.\n", MinBaseline)
		}
		b.WriteString("\n")
	}

	if len(s.DTCs) > 0 || s.Readiness != nil {
		b.WriteString("## Trouble codes and readiness\n\n")
		if len(s.DTCs) == 0 {
			b.WriteString("No trouble codes.\n")
		}
		for _, d := range s.DTCs {
			fmt.Fprintf(&b, "- `%s` (%s)\n", d.Code, d.Kind)
		}
		if r := s.Readiness; r != nil {
			fmt.Fprintf(&b, "\nMIL: %s. Monitors: ", map[bool]string{true: "**on**", false: "off"}[r.MIL])
			var ms []string
			for _, k := range sortedKeys(r.Monitors) {
				ms = append(ms, fmt.Sprintf("%s %s", k, r.Monitors[k]))
			}
			b.WriteString(strings.Join(ms, ", "))
			b.WriteString(".\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## All PIDs\n\n| PID | Unit | n | Min | Mean | Max |\n|---|---|---|---|---|---|\n")
	for _, name := range sortedKeys(s.PIDs) {
		st := s.PIDs[name]
		unit := ""
		if p, ok := obd.ByName(name); ok {
			unit = p.Unit
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %.2f | %.2f | %.2f |\n", name, unit, st.N, st.Min, st.Mean, st.Max)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteTrendReport renders a Markdown report of metric trends across drives.
func WriteTrendReport(w io.Writer, vehicle string, drives []Summary, trends []TrendLine, aiAnalysis string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Trends: %s\n\n", vehicle)
	if len(drives) > 0 {
		fmt.Fprintf(&b, "%d drives from %s to %s.\n\n", len(drives),
			drives[0].Start.Local().Format("2006-01-02"), drives[len(drives)-1].Start.Local().Format("2006-01-02"))
	}
	if aiAnalysis != "" {
		b.WriteString("## Analysis\n\n" + strings.TrimSpace(aiAnalysis) + "\n\n")
	}
	b.WriteString("## Metrics\n\n| Metric | n | First | Last | Mean | Slope / drive | Slope / 30 days |\n|---|---|---|---|---|---|---|\n")
	for _, t := range trends {
		per30 := "–"
		if t.SlopePer30d != nil {
			per30 = fmt.Sprintf("%+.3f", *t.SlopePer30d)
		}
		fmt.Fprintf(&b, "| %s | %d | %.2f | %.2f | %.2f | %+.3f | %s |\n", t.Metric, t.N, t.First, t.Last, t.Mean, t.SlopePerDrive, per30)
	}
	b.WriteString("\n## Drives\n\n| Session | Date | Duration | km | Events |\n|---|---|---|---|---|\n")
	for _, d := range drives {
		fmt.Fprintf(&b, "| %d | %s | %s | %.1f | %d |\n", d.SessionID, d.Start.Local().Format(time.DateTime), d.Duration, d.DistanceKm, len(d.Events))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
