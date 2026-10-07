package telemetry

import (
	"fmt"
	"sort"
	"time"
)

// AlignmentStatus explains why a snapshot can or cannot be produced for a station.
type AlignmentStatus string

const (
	// StatusAligned means every required kind shares the resolved watermark.
	StatusAligned AlignmentStatus = "aligned"
	// StatusIncomplete means the watermark is known to the station but at least
	// one required kind has no sample at it.
	StatusIncomplete AlignmentStatus = "incomplete"
	// StatusUnknownWatermark means no required kind has ever reported the
	// requested watermark (within the time window).
	StatusUnknownWatermark AlignmentStatus = "unknown_watermark"
	// StatusNoData means the station has no hydraulic samples in the window.
	StatusNoData AlignmentStatus = "no_data"
)

// hydraulicKinds are the four metrics that must share a watermark for a
// HydraulicSnapshot to exist. Order is fixed so sources and gaps are reported
// deterministically.
var hydraulicKinds = []Kind{KindSupplyPressure, KindReturnPressure, KindFlow, KindValvePosition}

// QueryFilter narrows a snapshot query. Zero Watermark means "resolve the
// latest aligned watermark"; zero From/To leave the corresponding side of the
// window open.
type QueryFilter struct {
	StationIDs []string
	Watermark  uint64
	From       time.Time
	To         time.Time
}

// TimeWindow is the echoed sample-time window applied to a report.
type TimeWindow struct {
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
}

// SnapshotSource identifies the exact sample backing one metric of a snapshot.
type SnapshotSource struct {
	Kind      Kind      `json:"kind"`
	Watermark uint64    `json:"watermark"`
	Value     float64   `json:"value"`
	At        time.Time `json:"at"`
}

// MissingMetric describes a required kind that has no sample at the resolved
// watermark, and how far that kind has actually advanced within the window.
type MissingMetric struct {
	Kind            Kind       `json:"kind"`
	LatestWatermark uint64     `json:"latest_watermark"`
	LatestAt        *time.Time `json:"latest_at,omitempty"`
}

// SnapshotReport is the immutable alignment answer for one station. It is
// computed from a point-in-time copy of the series, so samples arriving after
// the query never alter it.
type SnapshotReport struct {
	StationID           string             `json:"station_id"`
	Status              AlignmentStatus    `json:"status"`
	RequestedWatermark  uint64             `json:"requested_watermark,omitempty"`
	Watermark           uint64             `json:"watermark"`
	AvailableWatermarks []uint64           `json:"available_watermarks"`
	Snapshot            *HydraulicSnapshot `json:"snapshot,omitempty"`
	Sources             []SnapshotSource   `json:"sources,omitempty"`
	Missing             []MissingMetric    `json:"missing,omitempty"`
	Window              TimeWindow         `json:"window"`
	QueriedAt           time.Time          `json:"queried_at"`
}

// Query answers alignment questions over a Series.
type Query struct {
	series *Series
	now    func() time.Time
}

func NewQuery(series *Series) *Query {
	return &Query{series: series, now: func() time.Time { return time.Now().UTC() }}
}

// Run returns one report per requested station (or every known station when
// the filter names none). Each report is computed from a deep copy taken
// under the series read lock, so concurrent Add calls cannot change a result
// while it is being built or after it is returned.
func (q *Query) Run(filter QueryFilter) ([]SnapshotReport, error) {
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.To.Before(filter.From) {
		return nil, fmt.Errorf("telemetry query window ends (%s) before it starts (%s)",
			filter.To.Format(time.RFC3339), filter.From.Format(time.RFC3339))
	}
	stationIDs := append([]string(nil), filter.StationIDs...)
	if len(stationIDs) == 0 {
		stationIDs = q.series.StationIDs()
	}
	reports := make([]SnapshotReport, 0, len(stationIDs))
	for _, stationID := range stationIDs {
		reports = append(reports, q.reportFor(stationID, filter))
	}
	return reports, nil
}

func (q *Query) reportFor(stationID string, filter QueryFilter) SnapshotReport {
	window := TimeWindow{}
	if !filter.From.IsZero() {
		from := filter.From
		window.From = &from
	}
	if !filter.To.IsZero() {
		to := filter.To
		window.To = &to
	}
	report := SnapshotReport{
		StationID:           stationID,
		RequestedWatermark:  filter.Watermark,
		AvailableWatermarks: []uint64{},
		Window:              window,
		QueriedAt:           q.now(),
	}

	// Snapshot the station's data: everything below runs on the copy.
	data := q.series.CopyStation(stationID)
	filtered := make(map[Kind][]Sample, len(hydraulicKinds))
	totalSamples := 0
	for _, kind := range hydraulicKinds {
		items := withinWindow(data[kind], filter.From, filter.To)
		filtered[kind] = items
		totalSamples += len(items)
	}
	if totalSamples == 0 {
		report.Status = StatusNoData
		report.Missing = missingMetrics(filtered)
		return report
	}

	aligned := commonWatermarks(filtered, hydraulicKinds)
	report.AvailableWatermarks = aligned

	target := filter.Watermark
	if target == 0 {
		if len(aligned) == 0 {
			report.Status = StatusIncomplete
			report.Missing = missingMetrics(filtered)
			return report
		}
		target = aligned[len(aligned)-1]
	}
	report.Watermark = target

	present := make(map[Kind]Sample, len(hydraulicKinds))
	missing := make([]MissingMetric, 0)
	anyKindAtTarget := false
	for _, kind := range hydraulicKinds {
		if sample, ok := sampleAt(filtered[kind], target); ok {
			present[kind] = sample
			anyKindAtTarget = true
			continue
		}
		missing = append(missing, MissingMetric{Kind: kind})
	}
	if len(missing) > 0 {
		if anyKindAtTarget {
			report.Status = StatusIncomplete
		} else {
			report.Status = StatusUnknownWatermark
		}
		report.Missing = enrichMissing(missing, filtered)
		return report
	}

	snapshot, sources := assembleSnapshot(stationID, target, present)
	report.Status = StatusAligned
	report.Snapshot = &snapshot
	report.Sources = sources
	return report
}

func withinWindow(items []Sample, from, to time.Time) []Sample {
	if from.IsZero() && to.IsZero() {
		return items
	}
	kept := make([]Sample, 0, len(items))
	for _, item := range items {
		if !from.IsZero() && item.At.Before(from) {
			continue
		}
		if !to.IsZero() && item.At.After(to) {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// commonWatermarks returns the sorted watermarks present for every kind.
func commonWatermarks(data map[Kind][]Sample, kinds []Kind) []uint64 {
	counts := make(map[uint64]int)
	for _, kind := range kinds {
		seen := make(map[uint64]struct{})
		for _, sample := range data[kind] {
			seen[sample.Watermark] = struct{}{}
		}
		for watermark := range seen {
			counts[watermark]++
		}
	}
	common := make([]uint64, 0)
	for watermark, count := range counts {
		if count == len(kinds) {
			common = append(common, watermark)
		}
	}
	sort.Slice(common, func(i, j int) bool { return common[i] < common[j] })
	return common
}

func sampleAt(items []Sample, watermark uint64) (Sample, bool) {
	index := sort.Search(len(items), func(i int) bool { return items[i].Watermark >= watermark })
	if index >= len(items) || items[index].Watermark != watermark {
		return Sample{}, false
	}
	return items[index], true
}

func missingMetrics(data map[Kind][]Sample) []MissingMetric {
	missing := make([]MissingMetric, 0, len(hydraulicKinds))
	for _, kind := range hydraulicKinds {
		missing = append(missing, MissingMetric{Kind: kind})
	}
	return enrichMissing(missing, data)
}

func enrichMissing(missing []MissingMetric, data map[Kind][]Sample) []MissingMetric {
	for index := range missing {
		items := data[missing[index].Kind]
		if len(items) == 0 {
			continue
		}
		latest := items[len(items)-1]
		missing[index].LatestWatermark = latest.Watermark
		at := latest.At
		missing[index].LatestAt = &at
	}
	return missing
}

func assembleSnapshot(stationID string, watermark uint64, present map[Kind]Sample) (HydraulicSnapshot, []SnapshotSource) {
	sources := make([]SnapshotSource, 0, len(hydraulicKinds))
	captured := time.Time{}
	for _, kind := range hydraulicKinds {
		sample := present[kind]
		sources = append(sources, SnapshotSource{
			Kind: kind, Watermark: sample.Watermark, Value: sample.Value, At: sample.At,
		})
		if sample.At.After(captured) {
			captured = sample.At
		}
	}
	return HydraulicSnapshot{
		StationID:     stationID,
		Watermark:     watermark,
		Supply:        present[KindSupplyPressure].Value,
		Return:        present[KindReturnPressure].Value,
		Flow:          present[KindFlow].Value,
		ValvePosition: present[KindValvePosition].Value,
		CapturedAt:    captured,
		Sources:       sources,
	}, sources
}
