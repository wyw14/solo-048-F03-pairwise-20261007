package telemetry

import (
	"testing"
	"time"
)

var hydraulicKindList = []Kind{KindSupplyPressure, KindReturnPressure, KindFlow, KindValvePosition}

func addAt(t *testing.T, series *Series, stationID string, watermark uint64, at time.Time, kinds ...Kind) {
	t.Helper()
	if len(kinds) == 0 {
		kinds = hydraulicKindList
	}
	for index, kind := range kinds {
		sample := Sample{
			StationID: stationID, Kind: kind, Watermark: watermark,
			Value: float64(watermark) + float64(index)/10, At: at,
		}
		if err := series.Add(sample); err != nil {
			t.Fatalf("add %s @ %d: %v", kind, watermark, err)
		}
	}
}

func alignedSeries(t *testing.T) *Series {
	t.Helper()
	series := NewSeries(64)
	base := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	addAt(t, series, "xincheng", 1, base)
	addAt(t, series, "xincheng", 2, base.Add(time.Minute))
	addAt(t, series, "binhe", 1, base)
	return series
}

func TestQueryLatestAndHistoricalWatermark(t *testing.T) {
	query := NewQuery(alignedSeries(t))

	latest, err := query.Run(QueryFilter{StationIDs: []string{"xincheng"}})
	if err != nil {
		t.Fatalf("run latest: %v", err)
	}
	if latest[0].Status != StatusAligned || latest[0].Watermark != 2 {
		t.Fatalf("latest = status %s watermark %d, want aligned @2", latest[0].Status, latest[0].Watermark)
	}

	historical, err := query.Run(QueryFilter{StationIDs: []string{"xincheng"}, Watermark: 1})
	if err != nil {
		t.Fatalf("run historical: %v", err)
	}
	report := historical[0]
	if report.Status != StatusAligned || report.Watermark != 1 {
		t.Fatalf("historical = status %s watermark %d, want aligned @1", report.Status, report.Watermark)
	}
	if report.Snapshot == nil || report.Snapshot.StationID != "xincheng" {
		t.Fatalf("historical snapshot missing: %+v", report.Snapshot)
	}
	if got := report.AvailableWatermarks; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("available watermarks = %v, want [1 2]", got)
	}
	if len(report.Sources) != 4 {
		t.Fatalf("sources = %d, want 4", len(report.Sources))
	}
	for _, source := range report.Sources {
		if source.Watermark != 1 || source.At.IsZero() {
			t.Fatalf("source not pinned to watermark 1: %+v", source)
		}
	}
}

func TestQueryMissingMetrics(t *testing.T) {
	series := alignedSeries(t)
	// Watermark 3 only has supply pressure and flow; return and valve lag behind.
	addAt(t, series, "xincheng", 3, time.Date(2026, 10, 6, 10, 2, 0, 0, time.UTC),
		KindSupplyPressure, KindFlow)

	reports, err := NewQuery(series).Run(QueryFilter{StationIDs: []string{"xincheng"}, Watermark: 3})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	report := reports[0]
	if report.Status != StatusIncomplete {
		t.Fatalf("status = %s, want incomplete", report.Status)
	}
	if len(report.Missing) != 2 {
		t.Fatalf("missing = %+v, want 2 metrics", report.Missing)
	}
	wantMissing := map[Kind]bool{KindReturnPressure: false, KindValvePosition: false}
	for _, metric := range report.Missing {
		if _, ok := wantMissing[metric.Kind]; !ok {
			t.Fatalf("unexpected missing metric %s", metric.Kind)
		}
		wantMissing[metric.Kind] = true
		// These kinds last reported at watermark 2; the report must say so.
		if metric.LatestWatermark != 2 {
			t.Fatalf("%s latest watermark = %d, want 2", metric.Kind, metric.LatestWatermark)
		}
		if metric.LatestAt == nil {
			t.Fatalf("%s latest_at missing", metric.Kind)
		}
	}
	for kind, found := range wantMissing {
		if !found {
			t.Fatalf("expected %s in missing list", kind)
		}
	}
	if report.Snapshot != nil {
		t.Fatalf("incomplete report must not carry a snapshot")
	}
}

func TestQueryUnknownWatermark(t *testing.T) {
	reports, err := NewQuery(alignedSeries(t)).Run(QueryFilter{StationIDs: []string{"xincheng"}, Watermark: 99})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reports[0].Status != StatusUnknownWatermark {
		t.Fatalf("status = %s, want unknown_watermark", reports[0].Status)
	}
}

func TestQueryNoData(t *testing.T) {
	reports, err := NewQuery(alignedSeries(t)).Run(QueryFilter{StationIDs: []string{"ghost-station"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	report := reports[0]
	if report.Status != StatusNoData {
		t.Fatalf("status = %s, want no_data", report.Status)
	}
	if len(report.Missing) != 4 {
		t.Fatalf("no_data report should list all 4 metrics, got %d", len(report.Missing))
	}
}

func TestQueryTimeRange(t *testing.T) {
	query := NewQuery(alignedSeries(t))
	base := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

	// Window ending before watermark 2 was captured.
	reports, err := query.Run(QueryFilter{
		StationIDs: []string{"xincheng"}, To: base.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reports[0].Status != StatusAligned || reports[0].Watermark != 1 {
		t.Fatalf("bounded latest = %s @%d, want aligned @1", reports[0].Status, reports[0].Watermark)
	}

	// Window in the far future: nothing qualifies.
	future, err := query.Run(QueryFilter{
		StationIDs: []string{"xincheng"}, From: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("run future: %v", err)
	}
	if future[0].Status != StatusNoData {
		t.Fatalf("future window status = %s, want no_data", future[0].Status)
	}

	if _, err := query.Run(QueryFilter{
		StationIDs: []string{"xincheng"},
		From:       base, To: base.Add(-time.Hour),
	}); err == nil {
		t.Fatal("inverted window must error")
	}
}

func TestQueryResultImmutableAfterNewSamples(t *testing.T) {
	series := alignedSeries(t)
	query := NewQuery(series)

	// Query a historical watermark and hold the result.
	before, err := query.Run(QueryFilter{StationIDs: []string{"xincheng"}, Watermark: 1})
	if err != nil {
		t.Fatalf("run before: %v", err)
	}
	held := before[0]

	// Also hold a latest-resolution result: it must freeze at watermark 2.
	latestBefore, err := query.Run(QueryFilter{StationIDs: []string{"xincheng"}})
	if err != nil {
		t.Fatalf("run latest before: %v", err)
	}

	// New samples arrive after the queries.
	addAt(t, series, "xincheng", 3, time.Date(2026, 10, 6, 10, 3, 0, 0, time.UTC))

	if held.Status != StatusAligned || held.Watermark != 1 || held.Snapshot == nil || held.Snapshot.Watermark != 1 {
		t.Fatalf("held historical report mutated: %+v", held)
	}
	if len(held.Sources) != 4 || held.Sources[0].Watermark != 1 {
		t.Fatalf("held sources mutated: %+v", held.Sources)
	}
	if got := held.AvailableWatermarks; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("held available watermarks mutated: %v", got)
	}
	previous := latestBefore[0]
	if previous.Watermark != 2 || previous.Snapshot == nil || previous.Snapshot.Watermark != 2 {
		t.Fatalf("held latest report changed after new samples: %+v", previous)
	}

	// A fresh query sees the new state; the old results stay as they were.
	after, err := query.Run(QueryFilter{StationIDs: []string{"xincheng"}})
	if err != nil {
		t.Fatalf("run after: %v", err)
	}
	if after[0].Watermark != 3 {
		t.Fatalf("fresh latest watermark = %d, want 3", after[0].Watermark)
	}
}

func TestQueryAllStations(t *testing.T) {
	reports, err := NewQuery(alignedSeries(t)).Run(QueryFilter{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(reports) != 2 || reports[0].StationID != "binhe" || reports[1].StationID != "xincheng" {
		t.Fatalf("reports = %+v, want binhe then xincheng", reports)
	}
}

func TestBuildAtAndLegacyErrors(t *testing.T) {
	builder := NewSnapshotBuilder(alignedSeries(t))

	historical, err := builder.BuildAt("xincheng", 1)
	if err != nil {
		t.Fatalf("build at 1: %v", err)
	}
	if historical.Watermark != 1 {
		t.Fatalf("historical watermark = %d, want 1", historical.Watermark)
	}
	wantCaptured := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	if !historical.CapturedAt.Equal(wantCaptured) {
		t.Fatalf("captured_at = %s, want sample time %s", historical.CapturedAt, wantCaptured)
	}
	if len(historical.Sources) != 4 {
		t.Fatalf("snapshot sources = %d, want 4", len(historical.Sources))
	}

	if _, err := builder.Build("ghost-station"); err == nil ||
		err.Error() != "station ghost-station has no aligned hydraulic telemetry" {
		t.Fatalf("legacy error changed: %v", err)
	}
	if !builder.Ready("xincheng") {
		t.Fatal("xincheng should be ready")
	}
	if builder.Ready("ghost-station") {
		t.Fatal("ghost-station should not be ready")
	}
}
