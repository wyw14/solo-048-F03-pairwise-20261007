package api

import (
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
	t.Cleanup(func() { _ = app.Close(t.Context()) })
	return app
}

func getAlignment(t *testing.T, app *App, target string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)

	var envelope struct {
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return response.Code, envelope.Data
}

func TestAlignmentEndpointValidatesInput(t *testing.T) {
	app := newTestApp(t)

	cases := map[string]int{
		"/api/v1/telemetry/alignment":                       http.StatusBadRequest,
		"/api/v1/telemetry/alignment?station_id=ghost":      http.StatusNotFound,
		"/api/v1/telemetry/alignment?station_id=xincheng&watermark=abc": http.StatusBadRequest,
		"/api/v1/telemetry/alignment?station_id=xincheng&watermark=0":   http.StatusBadRequest,
	}
	for target, expected := range cases {
		status, body := getAlignment(t, app, target)
		if status != expected {
			t.Fatalf("%s: expected %d, got %d (%v)", target, expected, status, body)
		}
	}

	badRange := "/api/v1/telemetry/alignment?station_id=xincheng" +
		"&from=2026-10-07T10:05:00Z&to=2026-10-07T10:00:00Z"
	if status, _ := getAlignment(t, app, badRange); status != http.StatusBadRequest {
		t.Fatalf("inverted time range: expected 400, got %d", status)
	}
}

func TestAlignmentEndpointReturnsFrozenReport(t *testing.T) {
	app := newTestApp(t)
	at := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	for _, kind := range []telemetry.Kind{
		telemetry.KindSupplyPressure, telemetry.KindReturnPressure, telemetry.KindFlow, telemetry.KindValvePosition,
	} {
		if err := app.Telemetry.Add(telemetry.Sample{StationID: "xincheng", Kind: kind, Watermark: 2, Value: 10, At: at}); err != nil {
			t.Fatal(err)
		}
	}

	target := "/api/v1/telemetry/alignment?station_id=xincheng"
	status, first := getAlignment(t, app, target)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if first["aligned"] != true {
		t.Fatalf("expected aligned report: %v", first)
	}
	if watermark, _ := first["watermark"].(float64); watermark != 2 {
		t.Fatalf("expected newest common watermark 2, got %v", first["watermark"])
	}
	source, _ := first["snapshot_source"].(map[string]any)
	if source["source"] != "live_series" {
		t.Fatalf("expected live_series source, got %v", source)
	}

	// A newer aligned watermark arrives after the first query.
	for _, kind := range []telemetry.Kind{
		telemetry.KindSupplyPressure, telemetry.KindReturnPressure, telemetry.KindFlow, telemetry.KindValvePosition,
	} {
		if err := app.Telemetry.Add(telemetry.Sample{StationID: "xincheng", Kind: kind, Watermark: 3, Value: 11, At: at.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}

	_, second := getAlignment(t, app, target)
	if watermark, _ := second["watermark"].(float64); watermark != 2 {
		t.Fatalf("queried result must remain frozen at 2, got %v", second["watermark"])
	}
	source, _ = second["snapshot_source"].(map[string]any)
	if source["source"] != "frozen_report" {
		t.Fatalf("expected frozen_report source on replay, got %v", source)
	}
}

func TestAlignmentEndpointPinnedHistoricalWatermark(t *testing.T) {
	app := newTestApp(t)
	status, body := getAlignment(t, app, "/api/v1/telemetry/alignment?station_id=xincheng&watermark=1")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if body["aligned"] != true || body["watermark"].(float64) != 1 {
		t.Fatalf("expected aligned watermark 1, got %v", body)
	}
	missing, _ := body["missing_metrics"].([]any)
	if len(missing) != 0 {
		t.Fatalf("expected no missing metrics, got %v", missing)
	}
}
