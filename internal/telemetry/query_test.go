package telemetry

import (
	"testing"
	"time"
)

func baseAt(minute int) time.Time {
	return time.Date(2026, 10, 7, 10, minute, 0, 0, time.UTC)
}

func addAllFour(t *testing.T, series *Series, station string, watermark uint64, at time.Time, supply, ret, flow, position float64) {
	t.Helper()
	samples := []Sample{
		{StationID: station, Kind: KindSupplyPressure, Watermark: watermark, Value: supply, At: at},
		{StationID: station, Kind: KindReturnPressure, Watermark: watermark, Value: ret, At: at},
		{StationID: station, Kind: KindFlow, Watermark: watermark, Value: flow, At: at},
		{StationID: station, Kind: KindValvePosition, Watermark: watermark, Value: position, At: at},
	}
	for _, sample := range samples {
		if err := series.Add(sample); err != nil {
			t.Fatalf("add %s at %d: %v", sample.Kind, watermark, err)
		}
	}
}

func newTestSeries(t *testing.T) *Series {
	t.Helper()
	series := NewSeries(128)
	addAllFour(t, series, "xincheng", 1, baseAt(0), 0.78, 0.46, 8.1, 72)
	addAllFour(t, series, "xincheng", 2, baseAt(1), 0.80, 0.47, 8.4, 74)
	addAllFour(t, series, "xincheng", 3, baseAt(2), 0.82, 0.48, 8.6, 75)
	// Watermark 4 is incomplete: flow never arrives.
	partial := []Sample{
		{StationID: "xincheng", Kind: KindSupplyPressure, Watermark: 4, Value: 0.83, At: baseAt(3)},
		{StationID: "xincheng", Kind: KindReturnPressure, Watermark: 4, Value: 0.49, At: baseAt(3)},
		{StationID: "xincheng", Kind: KindValvePosition, Watermark: 4, Value: 76, At: baseAt(3)},
	}
	for _, sample := range partial {
		if err := series.Add(sample); err != nil {
			t.Fatalf("add partial: %v", err)
		}
	}
	return series
}

func TestMaterializeLatestCommonWatermark(t *testing.T) {
	series := newTestSeries(t)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{StationID: "xincheng"})

	if !report.Aligned {
		t.Fatalf("expected aligned report, missing: %+v", report.Missing)
	}
	if report.Watermark != 3 {
		t.Fatalf("expected newest common watermark 3, got %d", report.Watermark)
	}
	if report.Snapshot == nil {
		t.Fatal("expected snapshot")
	}
	if report.Snapshot.Supply != 0.82 || report.Snapshot.Return != 0.48 || report.Snapshot.Flow != 8.6 || report.Snapshot.ValvePosition != 75 {
		t.Fatalf("snapshot values wrong: %+v", report.Snapshot)
	}
	if len(report.Provenance) != 4 {
		t.Fatalf("expected 4 provenance entries, got %d", len(report.Provenance))
	}
	for _, entry := range report.Provenance {
		if entry.Watermark != 3 {
			t.Fatalf("provenance watermark mismatch: %+v", entry)
		}
	}
	if report.Source.Source != "live_series" || !report.Source.Materialized {
		t.Fatalf("unexpected snapshot source: %+v", report.Source)
	}
}

func TestMaterializeHistoricalWatermark(t *testing.T) {
	series := newTestSeries(t)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{StationID: "xincheng", Watermark: 1})

	if !report.Aligned {
		t.Fatalf("expected aligned, missing: %+v", report.Missing)
	}
	if report.Watermark != 1 {
		t.Fatalf("expected watermark 1, got %d", report.Watermark)
	}
	if report.Snapshot.Flow != 8.1 {
		t.Fatalf("expected historical flow 8.1, got %v", report.Snapshot.Flow)
	}
}

func TestMaterializeIncompleteWatermarkReportsMissing(t *testing.T) {
	series := newTestSeries(t)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{StationID: "xincheng", Watermark: 4})

	if report.Aligned {
		t.Fatal("expected unaligned report at partial watermark")
	}
	if report.Snapshot != nil {
		t.Fatal("expected no snapshot when unaligned")
	}
	if len(report.Missing) != 1 {
		t.Fatalf("expected exactly 1 missing metric, got %+v", report.Missing)
	}
	missing := report.Missing[0]
	if missing.Kind != KindFlow || missing.Reason != "absent" {
		t.Fatalf("expected absent flow, got %+v", missing)
	}
	if len(report.Provenance) != 3 {
		t.Fatalf("expected provenance for the 3 present metrics, got %d", len(report.Provenance))
	}
}

func TestMaterializeUnknownWatermarkMarksStale(t *testing.T) {
	series := newTestSeries(t)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{StationID: "xincheng", Watermark: 99})

	if report.Aligned {
		t.Fatal("expected unaligned report")
	}
	for _, missing := range report.Missing {
		if missing.Reason != "stale" {
			t.Fatalf("expected stale reason for unknown future watermark, got %+v", missing)
		}
	}
}

func TestMaterializeEmptyStationAllAbsent(t *testing.T) {
	series := NewSeries(128)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{StationID: "ghost"})
	if report.Aligned || report.Watermark != 0 || len(report.Missing) != 4 {
		t.Fatalf("unexpected report for empty station: %+v", report)
	}
	for _, missing := range report.Missing {
		if missing.Reason != "absent" {
			t.Fatalf("expected absent, got %+v", missing)
		}
	}
}

func TestMaterializeTimeRangeFiltersCommonWatermark(t *testing.T) {
	series := newTestSeries(t)
	// Window ending before watermark 3 arrives: newest aligned must be 2.
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{
		StationID: "xincheng",
		From:      baseAt(0).Add(-time.Minute),
		To:        baseAt(1).Add(30 * time.Second),
	})
	if !report.Aligned || report.Watermark != 2 {
		t.Fatalf("expected aligned watermark 2 within window, got %+v", report)
	}
}

func TestMaterializePinnedWatermarkOutsideTimeRange(t *testing.T) {
	series := newTestSeries(t)
	report := NewAlignmentMaterializer(series).Materialize(AlignmentQuery{
		StationID: "xincheng", Watermark: 3,
		From: baseAt(3), To: baseAt(4),
	})
	if report.Aligned {
		t.Fatal("expected unaligned when all metrics are outside the window")
	}
	if len(report.Missing) != 4 {
		t.Fatalf("expected 4 out_of_range metrics, got %+v", report.Missing)
	}
	for _, missing := range report.Missing {
		if missing.Reason != "out_of_range" {
			t.Fatalf("expected out_of_range, got %+v", missing)
		}
	}
}

func TestArchiveFreezesReportAcrossNewSamples(t *testing.T) {
	series := newTestSeries(t)
	archive := NewReportArchive(NewAlignmentMaterializer(series))
	query := AlignmentQuery{StationID: "xincheng"}

	first := archive.Query(query)
	if !first.Aligned || first.Watermark != 3 {
		t.Fatalf("unexpected first report: %+v", first)
	}
	if first.Source.Source != "live_series" {
		t.Fatalf("expected first query to materialize, got %+v", first.Source)
	}

	// Complete watermark 4 after the query: newest common becomes 4.
	if err := series.Add(Sample{StationID: "xincheng", Kind: KindFlow, Watermark: 4, Value: 8.9, At: baseAt(4)}); err != nil {
		t.Fatal(err)
	}

	second := archive.Query(query)
	if second.Watermark != 3 {
		t.Fatalf("frozen report must stay at watermark 3, got %d", second.Watermark)
	}
	if second.Snapshot == nil || second.Snapshot.Flow != 8.6 {
		t.Fatalf("frozen snapshot values changed: %+v", second.Snapshot)
	}
	if second.Source.Source != "frozen_report" || second.Source.Materialized {
		t.Fatalf("expected frozen_report source, got %+v", second.Source)
	}
	if !second.Source.BuiltAt.Equal(first.Source.BuiltAt) {
		t.Fatalf("frozen built_at changed: first=%s second=%s", first.Source.BuiltAt, second.Source.BuiltAt)
	}
}

func TestArchiveFreezesUnalignedReport(t *testing.T) {
	series := newTestSeries(t)
	archive := NewReportArchive(NewAlignmentMaterializer(series))
	query := AlignmentQuery{StationID: "xincheng", Watermark: 4}

	first := archive.Query(query)
	if first.Aligned || len(first.Missing) != 1 {
		t.Fatalf("expected unaligned first report, got %+v", first)
	}
	if err := series.Add(Sample{StationID: "xincheng", Kind: KindFlow, Watermark: 4, Value: 8.9, At: baseAt(4)}); err != nil {
		t.Fatal(err)
	}
	second := archive.Query(query)
	if second.Aligned || len(second.Missing) != 1 {
		t.Fatalf("previously queried missing state must remain frozen, got %+v", second)
	}
}

func TestArchiveConcurrentQueriesWhileSamplesArrive(t *testing.T) {
	series := newTestSeries(t)
	archive := NewReportArchive(NewAlignmentMaterializer(series))
	query := AlignmentQuery{StationID: "xincheng"}

	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		for watermark := uint64(10); watermark < 60; watermark++ {
			samples := []Sample{
				{StationID: "xincheng", Kind: KindSupplyPressure, Watermark: watermark, Value: 1, At: baseAt(int(watermark))},
				{StationID: "xincheng", Kind: KindReturnPressure, Watermark: watermark, Value: 1, At: baseAt(int(watermark))},
				{StationID: "xincheng", Kind: KindFlow, Watermark: watermark, Value: 1, At: baseAt(int(watermark))},
				{StationID: "xincheng", Kind: KindValvePosition, Watermark: watermark, Value: 1, At: baseAt(int(watermark))},
			}
			for _, sample := range samples {
				if err := series.Add(sample); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}
	}()
	for index := 0; index < 200; index++ {
		report := archive.Query(query)
		if report.Aligned && report.Snapshot == nil {
			t.Fatalf("aligned report %d lacks snapshot", report.Watermark)
		}
	}
	<-done
	select {
	case err := <-errCh:
		t.Fatalf("concurrent add failed: %v", err)
	default:
	}

	// First query froze newest-common-at-that-time; it must never drift.
	frozen := archive.Query(query)
	if frozen.Source.Source != "frozen_report" {
		t.Fatalf("expected replay from frozen archive, got %+v", frozen.Source)
	}
}

func TestArchiveDistinctQueriesMaterializeIndependently(t *testing.T) {
	series := newTestSeries(t)
	archive := NewReportArchive(NewAlignmentMaterializer(series))

	latest := archive.Query(AlignmentQuery{StationID: "xincheng"})
	historical := archive.Query(AlignmentQuery{StationID: "xincheng", Watermark: 2})
	if latest.Watermark != 3 || historical.Watermark != 2 {
		t.Fatalf("query isolation broken: latest=%d historical=%d", latest.Watermark, historical.Watermark)
	}
	if historical.Source.Source != "live_series" {
		t.Fatalf("distinct query must materialize independently, got %+v", historical.Source)
	}
	// Same distinct query again is frozen.
	again := archive.Query(AlignmentQuery{StationID: "xincheng", Watermark: 2})
	if again.Source.Source != "frozen_report" {
		t.Fatalf("expected frozen replay, got %+v", again.Source)
	}
}
