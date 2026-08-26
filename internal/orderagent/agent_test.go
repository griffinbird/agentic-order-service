package orderagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/orderagent"
)

func TestNewFromEnvironmentValidatesConfigurationBeforeCredentialUse(t *testing.T) {
	t.Setenv("FOUNDRY_PROJECT_ENDPOINT", "")
	t.Setenv("FOUNDRY_MODEL", "")
	_, err := orderagent.NewFromEnvironment(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "FOUNDRY_PROJECT_ENDPOINT") {
		t.Fatalf("got %v, want endpoint validation error", err)
	}

	t.Setenv("FOUNDRY_PROJECT_ENDPOINT", "https://example.invalid/project")
	_, err = orderagent.NewFromEnvironment(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "FOUNDRY_MODEL") {
		t.Fatalf("got %v, want model validation error", err)
	}
}
