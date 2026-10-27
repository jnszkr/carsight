package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultModel is the Claude model used for analysis.
const DefaultModel = "claude-opus-5-5"

const driveSystemPrompt = `You are an experienced automotive diagnostic technician reviewing OBD-II data logged from a customer's car.

You receive a JSON summary of one drive (never raw samples): per-PID statistics, warm-up behaviour, warm idle and fuel trim figures, rule-based events, stored/pending DTCs, monitor readiness, and deltas against this car's own baseline of earlier drives (z-scores; |z| >= 2 with at least 3 baseline drives is flagged).

Write a concise Markdown report for the owner, with these sections:

### Health summary
Two to four sentences: overall condition on this drive and the single most important takeaway.

### Anomalies
A numbered list ranked by how much they matter (safety and engine damage first, then emissions/economy, then minor). For each: what was observed (with numbers), why it matters, and how confident you are. Say "None" if nothing stands out.

### Likely causes and what to check next
For each anomaly worth acting on, the most likely causes in order and concrete, inexpensive checks the owner or a mechanic can do.

### Trouble codes
Plain-language explanation of each DTC (what the code means, typical causes for this kind of car, urgency). Omit the section if there are none.

Guidance:
- Base conclusions on the data. Standard OBD-II PIDs are limited; say when something can't be determined from them.
- Distinguish genuine faults from normal behaviour (e.g. fast idle and open-loop fuelling while cold, O2 sensor not switching before warm-up, short drives that never fully warm up).
- Baseline deltas with few baseline drives are weak evidence; say so.
- Total fuel trim (STFT + LTFT) beyond ±10% is worth investigating; lean at idle but normal at cruise suggests a vacuum leak, lean everywhere suggests fuel delivery or MAF.
- Do not recommend clearing codes as a fix.`

const trendSystemPrompt = `You are an experienced automotive diagnostic technician reviewing how one car's OBD-II metrics have changed across many drives.

You receive JSON with per-drive key metrics (chronological), a least-squares slope per drive for each metric (plus a slope per 30 days when the drives span at least 3 days), and the events and DTCs seen per drive.

Write a concise Markdown report with these sections:

### Overview
Two to four sentences on the car's trajectory.

### Trends worth watching
Ranked list. For each: the metric, how it moved (numbers), whether the change is meaningful given drive-to-drive noise and number of drives, and what it could indicate (e.g. LTFT drifting positive: developing vacuum leak or weakening fuel delivery; warm-up time growing: thermostat; idle RPM rising: idle air control or vacuum leak; charging voltage falling: alternator/battery).

### Recommendations
Concrete next checks or maintenance, and what to keep monitoring.

Base conclusions on the data and say when the evidence is weak (few drives, inconsistent conditions).`

// Client asks Claude to interpret drive summaries.
type Client struct {
	api   anthropic.Client
	Model string
}

// NewClient creates a client. Credentials are resolved by the SDK
// (normally ANTHROPIC_API_KEY).
func NewClient(opts ...option.RequestOption) *Client {
	return &Client{api: anthropic.NewClient(opts...), Model: DefaultModel}
}

// DriveInput is what gets sent for a single-drive analysis.
type DriveInput struct {
	Drive    Summary `json:"drive"`
	Baseline []Delta `json:"baseline_deltas"`
}

// AnalyzeDrive returns Claude's Markdown analysis of one drive.
func (c *Client) AnalyzeDrive(ctx context.Context, s Summary, deltas []Delta) (string, error) {
	if deltas == nil {
		deltas = []Delta{}
	}
	return c.ask(ctx, driveSystemPrompt, DriveInput{Drive: s, Baseline: deltas})
}

// TrendInput is what gets sent for a multi-drive trend analysis.
type TrendInput struct {
	Vehicle string       `json:"vehicle"`
	Trends  []TrendLine  `json:"trends"`
	Drives  []TrendDrive `json:"drives"`
}

// TrendDrive is the per-drive slice of a trend analysis.
type TrendDrive struct {
	SessionID  int64              `json:"session_id"`
	Start      string             `json:"start"`
	Duration   Duration           `json:"duration"`
	DistanceKm float64            `json:"distance_km"`
	ColdStart  bool               `json:"cold_start"`
	Metrics    map[string]float64 `json:"metrics"`
	Events     []string           `json:"events"`
}

// AnalyzeTrends returns Claude's Markdown analysis of metric trends.
func (c *Client) AnalyzeTrends(ctx context.Context, vehicle string, drives []Summary, trends []TrendLine) (string, error) {
	in := TrendInput{Vehicle: vehicle, Trends: trends}
	for _, d := range drives {
		td := TrendDrive{SessionID: d.SessionID, Start: d.Start.Format("2006-01-02 15:04"), Duration: d.Duration,
			DistanceKm: d.DistanceKm, ColdStart: d.WarmUp.ColdStart, Metrics: d.Metrics, Events: []string{}}
		for _, e := range d.Events {
			td.Events = append(td.Events, e.Severity+": "+e.Detail)
		}
		in.Drives = append(in.Drives, td)
	}
	return c.ask(ctx, trendSystemPrompt, in)
}

func (c *Client) ask(ctx context.Context, system string, input any) (string, error) {
	data, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return "", err
	}
	msg, err := c.api.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.Model),
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("<drive_data>\n" + string(data) + "\n</drive_data>")),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortHigh},
		// If a safety classifier declines the request, the API re-serves it
		// on a fallback model within the same call.
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return "", fmt.Errorf("claude: API error %d: %w", apiErr.StatusCode, err)
		}
		return "", fmt.Errorf("claude: %w", err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return "", fmt.Errorf("claude: request declined (%s)", msg.StopDetails.Category)
	}
	var out strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	if msg.StopReason == anthropic.BetaStopReasonMaxTokens {
		out.WriteString("\n\n_(analysis truncated: output token limit reached)_")
	}
	if out.Len() == 0 {
		return "", errors.New("claude: empty response")
	}
	return out.String(), nil
}
