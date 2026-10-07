package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"heatpulse/internal/telemetry"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = app.Close(ctx)
	})
	return app
}

func TestTelemetrySnapshotsEndpointSeededData(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/telemetry/snapshots?station=xincheng", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data []struct {
			StationID string                    `json:"station_id"`
			Status    telemetry.AlignmentStatus `json:"status"`
			Watermark uint64                    `json:"watermark"`
			Snapshot  *struct {
				Watermark uint64 `json:"watermark"`
			} `json:"snapshot"`
			Sources []map[string]any `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("reports = %d, want 1", len(envelope.Data))
	}
	report := envelope.Data[0]
	if report.StationID != "xincheng" || report.Status != telemetry.StatusAligned || report.Watermark != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Snapshot == nil || report.Snapshot.Watermark != 1 || len(report.Sources) != 4 {
		t.Fatalf("aligned report missing snapshot/sources: %+v", report)
	}
}

func TestTelemetrySnapshotsEndpointRejectsBadParams(t *testing.T) {
	app := newTestApp(t)
	cases := map[string]string{
		"watermark":       "/api/v1/telemetry/snapshots?watermark=abc",
		"from":            "/api/v1/telemetry/snapshots?from=not-a-time",
		"inverted window": "/api/v1/telemetry/snapshots?from=2026-10-06T10:00:00Z&to=2026-10-06T09:00:00Z",
	}
	for name, target := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body = %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestStationSnapshotEndpointHistoricalWatermark(t *testing.T) {
	app := newTestApp(t)
	base := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	for _, kind := range []telemetry.Kind{
		telemetry.KindSupplyPressure, telemetry.KindReturnPressure,
		telemetry.KindFlow, telemetry.KindValvePosition,
	} {
		if err := app.Telemetry.Add(telemetry.Sample{
			StationID: "binhe", Kind: kind, Watermark: 2, Value: 1, At: base.Add(time.Minute),
		}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/stations/binhe/telemetry/snapshot?watermark=1", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			Status    telemetry.AlignmentStatus `json:"status"`
			Watermark uint64                    `json:"watermark"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Data.Status != telemetry.StatusAligned || envelope.Data.Watermark != 1 {
		t.Fatalf("historical query via API = %+v", envelope.Data)
	}
}
