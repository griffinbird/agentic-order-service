package ordertool

import (
	"context"
	"fmt"

	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

type GetOrderInput struct {
	OrderID string `json:"orderId" jsonschema:"The order identifier"`
}

type GetOrderOutput struct {
	Order orderdomain.Order `json:"order"`
}

func NewGetOrder(service orderdomain.OrderReader) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "get_order",
		Description: "Get the current state of an order.",
	}, func(ctx context.Context, input GetOrderInput) (GetOrderOutput, error) {
		order, err := service.GetOrder(ctx, input.OrderID)
		if err != nil {
			return GetOrderOutput{}, fmt.Errorf("get order %q: %w", input.OrderID, err)
		}
		return GetOrderOutput{Order: order}, nil
	})
}

type GetPaymentInput struct {
	PaymentID string `json:"paymentId" jsonschema:"The payment identifier from the order"`
}

type GetPaymentOutput struct {
	Payment orderdomain.Payment `json:"payment"`
}

func NewGetPayment(service orderdomain.PaymentReader) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "get_payment_status",
		Description: "Get the capture status of a payment.",
	}, func(ctx context.Context, input GetPaymentInput) (GetPaymentOutput, error) {
		payment, err := service.GetPayment(ctx, input.PaymentID)
		if err != nil {
			return GetPaymentOutput{}, fmt.Errorf("get payment %q: %w", input.PaymentID, err)
		}
		return GetPaymentOutput{Payment: payment}, nil
	})
}

type GetInventoryInput struct {
	SKU       string `json:"sku" jsonschema:"The product SKU"`
	Warehouse string `json:"warehouse" jsonschema:"The warehouse code"`
}

type GetInventoryOutput struct {
	Inventory orderdomain.Inventory `json:"inventory"`
}

func NewGetInventory(service orderdomain.InventoryReader) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "get_inventory",
		Description: "Get available inventory for a SKU at one warehouse.",
	}, func(ctx context.Context, input GetInventoryInput) (GetInventoryOutput, error) {
		inventory, err := service.GetInventory(ctx, input.SKU, input.Warehouse)
		if err != nil {
			return GetInventoryOutput{}, fmt.Errorf("get inventory for SKU %q at warehouse %q: %w", input.SKU, input.Warehouse, err)
		}
		return GetInventoryOutput{Inventory: inventory}, nil
	})
}

type GetFulfilmentInput struct {
	OrderID string `json:"orderId" jsonschema:"The order identifier"`
}

type GetFulfilmentOutput struct {
	Fulfilment orderdomain.Fulfilment `json:"fulfilment"`
}

func NewGetFulfilment(service orderdomain.FulfilmentReader) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name:        "get_fulfilment_status",
		Description: "Get the current fulfilment state for an order.",
	}, func(ctx context.Context, input GetFulfilmentInput) (GetFulfilmentOutput, error) {
		fulfilment, err := service.GetFulfilment(ctx, input.OrderID)
		if err != nil {
			return GetFulfilmentOutput{}, fmt.Errorf("get fulfilment for order %q: %w", input.OrderID, err)
		}
		return GetFulfilmentOutput{Fulfilment: fulfilment}, nil
	})
}

func ReadOnlyTools(service orderdomain.Services) []tool.Tool {
	return []tool.Tool{
		NewGetOrder(service),
		NewGetPayment(service),
		NewGetInventory(service),
		NewGetFulfilment(service),
	}
}
