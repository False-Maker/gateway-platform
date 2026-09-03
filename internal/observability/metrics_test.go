package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryExposesCountersAndGauges(t *testing.T) {
	registry := NewRegistry()
	registry.AddCounter("release_xadd_total", 1, "result", "success")
	registry.SetGauge("control_stream_pending", 2)
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{`release_xadd_total{result="success"} 1`, "control_stream_pending 2"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics output missing %q:\n%s", expected, body)
		}
	}
}
