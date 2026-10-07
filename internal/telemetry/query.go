package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// hydraulicKinds are the four metric kinds that must share a watermark for a
// snapshot to be aligned. Order is fixed so report output is deterministic.
var hydraulicKinds = []Kind{KindSupplyPressure, KindReturnPressure, KindFlow, KindValvePosition}

// AlignmentQuery selects a point-in-time alignment report.
type AlignmentQuery struct {
	StationID string
	// Watermark pins the report to a historical watermark. Zero resolves the
	// newest common watermark observed within [From, To].
	Watermark uint64
	// From/To bound sample observation times. Zero values are unbounded.
	From time.Time
	To   time.Time
}

// MetricProvenance records the exact sample that backs one snapshot metric.
type MetricProvenance struct {
	Kind       Kind      `json:"kind"`
	Watermark  uint64    `json:"watermark"`
	Value      float64   `json:"value"`
	RecordedAt time.Time `json:"recorded_at"`
}

// SnapshotProvenance identifies how the returned snapshot was assembled.
type SnapshotProvenance struct {
	Source string `json:"source"`
	// Materialized is true when the snapshot was freshly built from the
	// series, false when a previously frozen report was replayed.
	Materialized bool      `json:"materialized"`
	BuiltAt      time.Time `json:"built_at"`
}

// MissingMetric explains why one metric could not be aligned at the watermark.
type MissingMetric struct {
	Kind Kind `json:"kind"`
	// Reason is absent (other metrics reached the watermark but this one did
	// not), stale (the watermark is ahead of every metric observed) or
	// out_of_range (sample exists but outside the requested time range).
	Reason       string     `json:"reason"`
	Watermark    uint64     `json:"watermark,omitempty"`
	NearestAt    *time.Time `json:"nearest_at,omitempty"`
	ObservedFrom *time.Time `json:"observed_from,omitempty"`
	ObservedTo   *time.Time `json:"observed_to,omitempty"`
}

// AlignmentReport is the immutable result of one alignment query. Reports are
// frozen once materialized: later samples never alter a queried result.
type AlignmentReport struct {
	StationID  string             `json:"station_id"`
	Watermark  uint64             `json:"watermark"`
	Aligned    bool               `json:"aligned"`
	From       *time.Time         `json:"from,omitempty"`
	To         *time.Time         `json:"to,omitempty"`
	Missing    []MissingMetric    `json:"missing_metrics"`
	Provenance []MetricProvenance `json:"provenance"`
	Snapshot   *HydraulicSnapshot `json:"snapshot,omitempty"`
	Source     SnapshotProvenance `json:"snapshot_source"`
}

func (q AlignmentQuery) cacheKey() string {
	var builder strings.Builder
	builder.WriteString(q.StationID)
	builder.WriteByte('|')
	fmt.Fprintf(&builder, "%d|", q.Watermark)
	builder.WriteString(fmtTime(q.From))
	builder.WriteByte('|')
	builder.WriteString(fmtTime(q.To))
	return builder.String()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// AlignmentMaterializer builds alignment reports against a series under a
// single read lock, so the report reflects one consistent point-in-time view.
type AlignmentMaterializer struct {
	series *Series
}

func NewAlignmentMaterializer(series *Series) *AlignmentMaterializer {
	return &AlignmentMaterializer{series: series}
}

// Materialize computes the report. It never reports an error for missing
// telemetry: callers learn that from report.Aligned and report.Missing.
func (m *AlignmentMaterializer) Materialize(q AlignmentQuery) AlignmentReport {
	from, to := q.From.UTC(), q.To.UTC()
	report := AlignmentReport{
		StationID: q.StationID, Watermark: q.Watermark,
		Missing:    []MissingMetric{},
		Provenance: []MetricProvenance{},
		Source:     SnapshotProvenance{Source: "live_series", Materialized: true, BuiltAt: time.Now().UTC()},
	}
	if !from.IsZero() {
		report.From = &from
	}
	if !to.IsZero() {
		report.To = &to
	}

	m.series.mu.RLock()
	defer m.series.mu.RUnlock()

	// Copy the per-kind slices we read so the report cannot be mutated after
	// the lock is released.
	perKind := make(map[Kind][]Sample, len(hydraulicKinds))
	for _, kind := range hydraulicKinds {
		perKind[kind] = append([]Sample(nil), m.series.samples[q.StationID][kind]...)
	}

	target := q.Watermark
	if target == 0 {
		target = newestCommonWatermark(perKind, from, to)
		report.Watermark = target
	}

	highest := uint64(0)
	for _, kind := range hydraulicKinds {
		if items := perKind[kind]; len(items) > 0 && items[len(items)-1].Watermark > highest {
			highest = items[len(items)-1].Watermark
		}
	}

	if target == 0 {
		report.Aligned = false
		for _, kind := range hydraulicKinds {
			report.Missing = append(report.Missing, classifyMissing(kind, q.Watermark, perKind[kind], "absent", from, to))
		}
		return report
	}

	values := make(map[Kind]Sample, len(hydraulicKinds))
	for _, kind := range hydraulicKinds {
		sample, within, found := sampleAt(perKind[kind], target, from, to)
		if found && within {
			values[kind] = sample
			report.Provenance = append(report.Provenance, MetricProvenance{
				Kind: kind, Watermark: sample.Watermark, Value: sample.Value, RecordedAt: sample.At.UTC(),
			})
			continue
		}
		reason := "absent"
		switch {
		case found:
			// Sample exists at the watermark but its observation time is
			// outside the requested window.
			reason = "out_of_range"
		case target > highest:
			// Nothing observed this watermark on any metric: the station has
			// not caught up to it yet.
			reason = "stale"
		}
		report.Missing = append(report.Missing, classifyMissing(kind, target, perKind[kind], reason, from, to))
	}

	report.Aligned = len(report.Missing) == 0
	if !report.Aligned {
		return report
	}
	supply, ret, flow, position := values[KindSupplyPressure], values[KindReturnPressure], values[KindFlow], values[KindValvePosition]
	report.Snapshot = &HydraulicSnapshot{
		StationID: q.StationID, Watermark: target,
		Supply: supply.Value, Return: ret.Value, Flow: flow.Value, ValvePosition: position.Value,
		CapturedAt: latestOf(supply.At, ret.At, flow.At, position.At).UTC(),
	}
	return report
}

func newestCommonWatermark(perKind map[Kind][]Sample, from, to time.Time) uint64 {
	counts := make(map[uint64]int)
	for _, kind := range hydraulicKinds {
		seen := make(map[uint64]struct{})
		for _, sample := range perKind[kind] {
			if !withinRange(sample.At, from, to) {
				continue
			}
			seen[sample.Watermark] = struct{}{}
		}
		for watermark := range seen {
			counts[watermark]++
		}
	}
	var best uint64
	for watermark, count := range counts {
		if count == len(hydraulicKinds) && watermark > best {
			best = watermark
		}
	}
	return best
}

func sampleAt(items []Sample, watermark uint64, from, to time.Time) (Sample, bool, bool) {
	index := sort.Search(len(items), func(i int) bool { return items[i].Watermark >= watermark })
	if index >= len(items) || items[index].Watermark != watermark {
		return Sample{}, false, false
	}
	return items[index], withinRange(items[index].At, from, to), true
}

func withinRange(at time.Time, from, to time.Time) bool {
	if !from.IsZero() && at.Before(from) {
		return false
	}
	if !to.IsZero() && at.After(to) {
		return false
	}
	return true
}

// classifyMissing builds the missing entry for one kind, attaching the nearest
// available sample so callers can see how far the station has caught up.
func classifyMissing(kind Kind, requested uint64, items []Sample, reason string, from, to time.Time) MissingMetric {
	missing := MissingMetric{Kind: kind, Watermark: requested, Reason: reason}
	if !from.IsZero() {
		from = from.UTC()
		missing.ObservedFrom = &from
	}
	if !to.IsZero() {
		to = to.UTC()
		missing.ObservedTo = &to
	}
	for index := len(items) - 1; index >= 0; index-- {
		sample := items[index]
		if requested == 0 || sample.Watermark <= requested {
			nearest := sample.At.UTC()
			missing.NearestAt = &nearest
			missing.Watermark = sample.Watermark
			return missing
		}
	}
	return missing
}

func latestOf(times ...time.Time) time.Time {
	var latest time.Time
	for _, candidate := range times {
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

// alignmentArchiveLimit bounds frozen reports; eviction is LRU-ish by recency.
const alignmentArchiveLimit = 512

// ReportArchive wraps a materializer and freezes every queried report keyed by
// its query. New samples arriving after the first query never change the
// returned report.
type ReportArchive struct {
	materializer *AlignmentMaterializer

	mu      sync.Mutex
	reports map[string]AlignmentReport
	order   []string
}

func NewReportArchive(materializer *AlignmentMaterializer) *ReportArchive {
	return &ReportArchive{
		materializer: materializer,
		reports:      make(map[string]AlignmentReport),
	}
}

// Query returns the frozen report for q, materializing and freezing it on the
// first request. The returned pointer stays valid because reports are value
// types holding only copied slices.
func (a *ReportArchive) Query(q AlignmentQuery) AlignmentReport {
	key := q.cacheKey()
	a.mu.Lock()
	if report, ok := a.reports[key]; ok {
		a.touchLocked(key)
		report.Source = SnapshotProvenance{
			Source: "frozen_report", Materialized: false, BuiltAt: report.Source.BuiltAt,
		}
		a.mu.Unlock()
		return report
	}
	a.mu.Unlock()

	report := a.materializer.Materialize(q)

	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have materialized the same query concurrently.
	if existing, ok := a.reports[key]; ok {
		a.touchLocked(key)
		existing.Source = SnapshotProvenance{
			Source: "frozen_report", Materialized: false, BuiltAt: existing.Source.BuiltAt,
		}
		return existing
	}
	frozen := report
	a.reports[key] = frozen
	a.order = append(a.order, key)
	if len(a.order) > alignmentArchiveLimit {
		drop := a.order[:len(a.order)-alignmentArchiveLimit]
		a.order = a.order[len(drop):]
		for _, stale := range drop {
			delete(a.reports, stale)
		}
	}
	return frozen
}

func (a *ReportArchive) touchLocked(key string) {
	for index, entry := range a.order {
		if entry == key {
			a.order = append(a.order[:index], a.order[index+1:]...)
			a.order = append(a.order, key)
			return
		}
	}
}
