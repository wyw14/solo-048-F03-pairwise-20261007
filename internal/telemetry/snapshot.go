package telemetry

import (
	"fmt"
	"strings"
	"time"
)

type HydraulicSnapshot struct {
	StationID     string           `json:"station_id"`
	Watermark     uint64           `json:"watermark"`
	Supply        float64          `json:"supply_pressure"`
	Return        float64          `json:"return_pressure"`
	Flow          float64          `json:"flow"`
	ValvePosition float64          `json:"valve_position"`
	CapturedAt    time.Time        `json:"captured_at"`
	Sources       []SnapshotSource `json:"sources,omitempty"`
}

func (s HydraulicSnapshot) DifferentialPressure() float64 {
	return s.Supply - s.Return
}

type SnapshotBuilder struct {
	query *Query
}

func NewSnapshotBuilder(series *Series) *SnapshotBuilder {
	return &SnapshotBuilder{query: NewQuery(series)}
}

// Build returns the latest fully aligned snapshot for a station. It is kept as
// the narrow, error-returning entry point used by the hydraulic evaluation
// path; the richer alignment diagnostics live on Query.
func (b *SnapshotBuilder) Build(stationID string) (HydraulicSnapshot, error) {
	return b.BuildAt(stationID, 0)
}

// BuildAt returns the aligned snapshot at a specific historical watermark.
// Watermark 0 resolves the latest aligned one.
func (b *SnapshotBuilder) BuildAt(stationID string, watermark uint64) (HydraulicSnapshot, error) {
	reports, err := b.query.Run(QueryFilter{StationIDs: []string{stationID}, Watermark: watermark})
	if err != nil {
		return HydraulicSnapshot{}, err
	}
	report := reports[0]
	if report.Status == StatusAligned {
		return *report.Snapshot, nil
	}
	return HydraulicSnapshot{}, alignmentError(watermark, report)
}

func alignmentError(watermark uint64, report SnapshotReport) error {
	kinds := make([]string, 0, len(report.Missing))
	for _, metric := range report.Missing {
		kinds = append(kinds, string(metric.Kind))
	}
	missing := strings.Join(kinds, ", ")
	switch report.Status {
	case StatusNoData:
		return fmt.Errorf("station %s has no aligned hydraulic telemetry", report.StationID)
	case StatusUnknownWatermark:
		if watermark == 0 {
			return fmt.Errorf("station %s has no aligned hydraulic telemetry", report.StationID)
		}
		return fmt.Errorf("watermark %d is unknown to station %s", watermark, report.StationID)
	case StatusIncomplete:
		if watermark == 0 {
			return fmt.Errorf("station %s has no aligned hydraulic telemetry", report.StationID)
		}
		return fmt.Errorf("snapshot for station %s at watermark %d is missing metrics: %s",
			report.StationID, watermark, missing)
	default:
		return fmt.Errorf("station %s has no aligned hydraulic telemetry", report.StationID)
	}
}

func (b *SnapshotBuilder) Ready(stationID string) bool {
	_, err := b.Build(stationID)
	return err == nil
}
