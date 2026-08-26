package ordertool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/orderdata"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/griffinbird/agentic-order-service/internal/ordertool"
)

func TestReadOnlyToolSchemasAndCalls(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	tests := []struct {
		name string
		call func(context.Context, string) (any, error)
		args string
	}{
		{name: "get_order", call: ordertool.NewGetOrder(services).Call, args: `{"orderId":"58372"}`},
		{name: "get_payment_status", call: ordertool.NewGetPayment(services).Call, args: `{"paymentId":"PAY-58372"}`},
		{name: "get_inventory", call: ordertool.NewGetInventory(services).Call, args: `{"sku":"SKU-441","warehouse":"SYD"}`},
		{name: "get_fulfilment_status", call: ordertool.NewGetFulfilment(services).Call, args: `{"orderId":"58372"}`},
	}
	tools := ordertool.ReadOnlyTools(services)
	if len(tools) != len(tests) {
		t.Fatalf("got %d tools, want %d", len(tools), len(tests))
	}
	for i, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tools[i].Name() != tt.name {
				t.Fatalf("got tool %q, want %q", tools[i].Name(), tt.name)
			}
			result, err := tt.call(context.Background(), tt.args)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) == 0 || string(data) == "{}" {
				t.Fatalf("empty tool result: %s", data)
			}
		})
	}
}

func TestGetOrderToolRejectsInvalidInputAndPreservesErrors(t *testing.T) {
	t.Parallel()
	tool := ordertool.NewGetOrder(orderdata.NewDeterministicServices())
	if _, err := tool.Call(context.Background(), `{}`); err == nil {
		t.Fatal("expected schema validation error")
	}
	_, err := tool.Call(context.Background(), `{"orderId":"missing"}`)
	if !errors.Is(err, orderdomain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), `get order "missing"`) {
		t.Fatalf("missing operation context in %q", err)
	}
}

func TestGetOrderToolPreservesCancellation(t *testing.T) {
	t.Parallel()
	tool := ordertool.NewGetOrder(orderdata.NewDeterministicServices())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Call(ctx, `{"orderId":"58372"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
