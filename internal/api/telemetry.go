package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"heatpulse/internal/telemetry"
)

func (a *App) recordTelemetry(w http.ResponseWriter, r *http.Request) {
	var sample telemetry.Sample
	if err := decodeJSON(r, &sample); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.Telemetry.Add(sample); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sample)
}

func (a *App) evaluateHydraulic(w http.ResponseWriter, r *http.Request) {
	stationID := chi.URLParam(r, "stationID")
	snapshot, err := a.SnapshotsBuilder.Build(stationID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	balance, err := a.Balancer.Evaluate(snapshot)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	incident, err := a.LeakProtection.Apply(balance)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	var protectionIncident any
	var isolationSession any
	if incident != nil && incident.Confirmed {
		raised, raiseErr := a.Protection.Raise(stationID, "hydraulic_leak", "critical", "confirmed mass imbalance")
		if raiseErr != nil {
			writeError(w, http.StatusConflict, raiseErr)
			return
		}
		protectionIncident = raised
		stationState, ok := a.Stations.Get(stationID)
		if ok {
			sections := a.Topology.SectionsForZone(stationState.ZoneID)
			if len(sections) > 0 {
				session, isolateErr := a.IsolationCoordinator.FromIncident(r.Context(), *incident, sections[0].ID)
				if isolateErr != nil {
					writeError(w, http.StatusConflict, isolateErr)
					return
				}
				isolationSession = session
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snapshot, "balance": balance, "incident": incident,
		"protection_incident": protectionIncident, "isolation": isolationSession,
	})
}

// queryTelemetrySnapshots handles GET /api/v1/telemetry/snapshots and returns
// alignment diagnostics across stations. Query params: watermark (default
// latest aligned), from/to (RFC3339 sample-time bounds), station (repeatable
// or comma-separated). The result reflects a point-in-time copy of the series
// and never changes when new samples arrive afterwards.
func (a *App) queryTelemetrySnapshots(w http.ResponseWriter, r *http.Request) {
	filter, err := snapshotFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if raw := r.URL.Query()["station"]; len(raw) > 0 {
		for _, value := range raw {
			for _, stationID := range strings.Split(value, ",") {
				if stationID = strings.TrimSpace(stationID); stationID != "" {
					filter.StationIDs = append(filter.StationIDs, stationID)
				}
			}
		}
	}
	reports, err := a.TelemetryQuery.Run(filter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, reports)
}

// queryStationTelemetrySnapshot handles GET
// /api/v1/stations/{stationID}/telemetry/snapshot.
func (a *App) queryStationTelemetrySnapshot(w http.ResponseWriter, r *http.Request) {
	filter, err := snapshotFilterFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	filter.StationIDs = []string{chi.URLParam(r, "stationID")}
	reports, err := a.TelemetryQuery.Run(filter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, reports[0])
}

func snapshotFilterFromQuery(r *http.Request) (telemetry.QueryFilter, error) {
	filter := telemetry.QueryFilter{}
	values := r.URL.Query()
	if raw := values.Get("watermark"); raw != "" {
		watermark, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || watermark == 0 {
			return filter, errInvalid("watermark must be a positive integer")
		}
		filter.Watermark = watermark
	}
	if raw := values.Get("from"); raw != "" {
		from, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, errInvalid(`from must be an RFC3339 timestamp, e.g. ` + time.Now().Format(time.RFC3339))
		}
		filter.From = from
	}
	if raw := values.Get("to"); raw != "" {
		to, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, errInvalid(`to must be an RFC3339 timestamp, e.g. ` + time.Now().Format(time.RFC3339))
		}
		filter.To = to
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && filter.To.Before(filter.From) {
		return filter, errInvalid("to must not be before from")
	}
	return filter, nil
}

type badRequestError string

func (e badRequestError) Error() string { return string(e) }

func errInvalid(message string) error { return badRequestError(message) }
