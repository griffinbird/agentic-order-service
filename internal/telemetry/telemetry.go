package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/otelprovider"
	workflowobservability "github.com/microsoft/agent-framework-go/workflow/observability"
	workflowotel "github.com/microsoft/agent-framework-go/workflow/observability/opentelemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const EnvironmentVariable = "ORDER_DEMO_TELEMETRY"

type Runtime struct {
	provider        *sdktrace.TracerProvider
	AgentMiddleware agent.Middleware
	WorkflowTracer  workflowobservability.Tracer
}

func SetupFromEnvironment() (*Runtime, error) {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv(EnvironmentVariable)))
	if mode == "" || mode == "off" {
		return &Runtime{}, nil
	}
	if mode != "stdout" {
		return nil, fmt.Errorf("%s must be empty, off, or stdout", EnvironmentVariable)
	}
	exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, fmt.Errorf("create stdout trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(provider)
	return &Runtime{
		provider: provider,
		AgentMiddleware: otelprovider.NewMiddleware(otelprovider.MiddlewareConfig{
			SourceName: "agentic-order-service",
		}),
		WorkflowTracer: workflowotel.New(workflowotel.Config{
			SourceName: "agentic-order-service.workflow",
		}),
	}, nil
}

func (r *Runtime) AgentMiddlewares() []agent.Middleware {
	if r == nil || r.AgentMiddleware == nil {
		return nil
	}
	return []agent.Middleware{r.AgentMiddleware}
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}
