package mcpserver

const drivePrompt = `Act as an experienced automotive diagnostic technician and review %s from carsight.

Call get_drive_summary first. Look at raw samples with get_samples only where the summary leaves a question open (for example the shape of the warm-up curve, what RPM and trims did around an event, or whether the upstream O2 sensor switches).

Write a concise Markdown report for the owner:

### Health summary
Two to four sentences: overall condition on this drive and the single most important takeaway.

### Anomalies
A numbered list ranked by how much they matter (safety and engine damage first, then emissions/economy, then minor). For each: what was observed (with numbers), why it matters, and how confident you are. Say "None" if nothing stands out.

### Likely causes and what to check next
For each anomaly worth acting on, the most likely causes in order and concrete, inexpensive checks the owner or a mechanic can do.

### Trouble codes
Plain-language explanation of each DTC (what it means, typical causes for this kind of car, urgency). Omit if there are none.

Guidance:
- Base conclusions on the data. Standard OBD-II PIDs are limited; say when something can't be determined from them.
- Distinguish faults from normal behaviour: fast idle and open-loop fuelling while cold, O2 sensor not switching before warm-up, short drives that never fully warm up.
- Baseline deltas with few baseline drives are weak evidence; say so.
- Total fuel trim (STFT + LTFT) beyond ±10%% is worth investigating; lean at idle but normal at cruise suggests a vacuum leak, lean everywhere suggests fuel delivery or the MAF.
- Do not recommend clearing codes as a fix.`

const trendPrompt = `Act as an experienced automotive diagnostic technician and review how %s has changed across its logged drives.

Call get_trends (list_vehicles first if you don't know the VIN). Drill into individual drives with get_drive_summary or get_samples where a change needs explaining.

Write a concise Markdown report:

### Overview
Two to four sentences on the car's trajectory.

### Trends worth watching
Ranked list. For each: the metric, how it moved (numbers), whether the change is meaningful given drive-to-drive noise and the number of drives, and what it could indicate (e.g. LTFT drifting positive: developing vacuum leak or weakening fuel delivery; warm-up time growing: thermostat; idle RPM rising: idle air control or vacuum leak; charging voltage falling: alternator or battery).

### Recommendations
Concrete next checks or maintenance, and what to keep monitoring.

Say when the evidence is weak (few drives, inconsistent conditions such as warm vs cold starts).`
