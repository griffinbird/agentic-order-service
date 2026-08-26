package shippingmcp_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/griffinbird/agentic-order-service/internal/shippingmcp"
)

func TestMCPServerClientIntegration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(shippingmcp.NewHTTPHandler(shippingmcp.NewServer(shippingmcp.NewService())))
	defer server.Close()

	client, err := shippingmcp.Connect(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close MCP client: %v", err)
		}
	}()

	var names []string
	for _, candidate := range client.Tools() {
		names = append(names, candidate.Name())
	}
	slices.Sort(names)
	want := []string{shippingmcp.EstimateDeliveryTool, shippingmcp.GetShippingStatusTool}
	if !slices.Equal(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}

	estimate, err := client.EstimateDelivery(context.Background(), orderdomain.SydneyWarehouse, orderdomain.DefaultDestination)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.AdditionalDays != 1 || estimate.AdditionalCostCents != 840 {
		t.Fatalf("unexpected estimate: %+v", estimate)
	}

	status, err := client.GetShippingStatus(context.Background(), "SHP-58370")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "in_transit" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestMCPToolErrorIsExplicit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(shippingmcp.NewHTTPHandler(shippingmcp.NewServer(shippingmcp.NewService())))
	defer server.Close()
	client, err := shippingmcp.Connect(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.EstimateDelivery(context.Background(), "UNKNOWN", orderdomain.DefaultDestination); err == nil {
		t.Fatal("expected unknown origin error")
	}
}
