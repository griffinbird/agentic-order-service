package telemetry_test

import (
	"context"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/telemetry"
)

func TestTelemetryIsOptIn(t *testing.T) {
	t.Setenv(telemetry.EnvironmentVariable, "")
	runtime, err := telemetry.SetupFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.AgentMiddlewares()) != 0 || runtime.WorkflowTracer != nil {
		t.Fatal("telemetry should be disabled by default")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
