package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"heatpulse/internal/telemetry"
)

// queryAlignment serves point-in-time alignment reports:
// GET /api/v1/telemetry/alignment?station_id=...&watermark=...&from=...&to=...
// A report is frozen on first query, so telemetry arriving later never changes
// the response for the same parameters.
func (a *App) queryAlignment(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()

	stationID := values.Get("station_id")
	if stationID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("station_id query parameter is required"))
		return
	}
	if _, ok := a.Stations.Get(stationID); !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("station %s not found", stationID))
		return
	}

	query := telemetry.AlignmentQuery{StationID: stationID}
	if raw := values.Get("watermark"); raw != "" {
		watermark, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || watermark == 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("watermark must be a positive integer"))
			return
		}
		query.Watermark = watermark
	}

	var err error
	if query.From, err = parseAlignmentTime(values.Get("from")); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid from: %w", err))
		return
	}
	if query.To, err = parseAlignmentTime(values.Get("to")); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid to: %w", err))
		return
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.To.Before(query.From) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("to (%s) must not be before from (%s)", query.To.Format(time.RFC3339), query.From.Format(time.RFC3339)))
		return
	}

	writeJSON(w, http.StatusOK, a.AlignmentReports.Query(query))
}

func parseAlignmentTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339 timestamp, got %q", raw)
	}
	return parsed.UTC(), nil
}
