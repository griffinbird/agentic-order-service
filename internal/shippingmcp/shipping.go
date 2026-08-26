package shippingmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/mcptool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	GetShippingStatusTool = "get_shipping_status"
	EstimateDeliveryTool  = "estimate_delivery"
)

type GetShippingStatusInput struct {
	ShipmentID string `json:"shipmentId" jsonschema:"The shipment identifier"`
}

type GetShippingStatusOutput struct {
	Status orderdomain.ShippingStatus `json:"status"`
}

type EstimateDeliveryInput struct {
	Origin      string `json:"origin" jsonschema:"The origin warehouse code"`
	Destination string `json:"destination" jsonschema:"The delivery destination"`
}

type EstimateDeliveryOutput struct {
	Estimate orderdomain.DeliveryEstimate `json:"estimate"`
}

type Service struct{}

func NewService() *Service {
	return &Service{}
}

func (s *Service) GetShippingStatus(ctx context.Context, shipmentID string) (orderdomain.ShippingStatus, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.ShippingStatus{}, err
	}
	if err := orderdomain.ValidateID("shipment ID", shipmentID); err != nil {
		return orderdomain.ShippingStatus{}, err
	}
	if shipmentID != "SHP-58370" {
		return orderdomain.ShippingStatus{}, fmt.Errorf("shipment %q: %w", shipmentID, orderdomain.ErrNotFound)
	}
	return orderdomain.ShippingStatus{
		ShipmentID: shipmentID,
		Status:     "in_transit",
		Detail:     "Departed Melbourne depot.",
	}, nil
}

func (s *Service) EstimateDelivery(ctx context.Context, origin, destination string) (orderdomain.DeliveryEstimate, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.DeliveryEstimate{}, err
	}
	if err := orderdomain.ValidateID("origin", origin); err != nil {
		return orderdomain.DeliveryEstimate{}, err
	}
	if err := orderdomain.ValidateID("destination", destination); err != nil {
		return orderdomain.DeliveryEstimate{}, err
	}
	switch origin {
	case orderdomain.MelbourneWarehouse:
		return orderdomain.DeliveryEstimate{
			Origin: origin, Destination: destination, EstimatedDeliveryDate: "2026-08-28",
			AdditionalDays: 0, AdditionalCostCents: 0, Currency: "AUD",
		}, nil
	case orderdomain.SydneyWarehouse:
		return orderdomain.DeliveryEstimate{
			Origin: origin, Destination: destination, EstimatedDeliveryDate: "2026-08-29",
			AdditionalDays: 1, AdditionalCostCents: 840, Currency: "AUD",
		}, nil
	default:
		return orderdomain.DeliveryEstimate{}, fmt.Errorf("origin %q: %w", origin, orderdomain.ErrNotFound)
	}
}

func NewServer(service *Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "order-demo-shipping",
		Version: "1.0.0",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        GetShippingStatusTool,
		Description: "Get the current status of a shipment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input GetShippingStatusInput) (*mcp.CallToolResult, GetShippingStatusOutput, error) {
		status, err := service.GetShippingStatus(ctx, input.ShipmentID)
		if err != nil {
			return nil, GetShippingStatusOutput{}, err
		}
		return nil, GetShippingStatusOutput{Status: status}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        EstimateDeliveryTool,
		Description: "Estimate delivery impact and cost from one origin.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input EstimateDeliveryInput) (*mcp.CallToolResult, EstimateDeliveryOutput, error) {
		estimate, err := service.EstimateDelivery(ctx, input.Origin, input.Destination)
		if err != nil {
			return nil, EstimateDeliveryOutput{}, err
		}
		return nil, EstimateDeliveryOutput{Estimate: estimate}, nil
	})
	return server
}

func NewHTTPHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})
}

type Client struct {
	session *mcp.ClientSession
	tools   []tool.Tool
}

func Connect(ctx context.Context, endpoint string) (*Client, error) {
	if err := orderdomain.ValidateID("shipping MCP endpoint", endpoint); err != nil {
		return nil, err
	}
	session, err := mcptool.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint})
	if err != nil {
		return nil, fmt.Errorf("connect to shipping MCP server: %w", err)
	}
	tools, err := mcptool.ListTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("list shipping MCP tools: %w", err)
	}
	return &Client{session: session, tools: tools}, nil
}

func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

func (c *Client) Tools() []tool.Tool {
	return append([]tool.Tool(nil), c.tools...)
}

func (c *Client) GetShippingStatus(ctx context.Context, shipmentID string) (orderdomain.ShippingStatus, error) {
	var output GetShippingStatusOutput
	if err := c.call(ctx, GetShippingStatusTool, GetShippingStatusInput{ShipmentID: shipmentID}, &output); err != nil {
		return orderdomain.ShippingStatus{}, err
	}
	return output.Status, nil
}

func (c *Client) EstimateDelivery(ctx context.Context, origin, destination string) (orderdomain.DeliveryEstimate, error) {
	var output EstimateDeliveryOutput
	if err := c.call(ctx, EstimateDeliveryTool, EstimateDeliveryInput{
		Origin: origin, Destination: destination,
	}, &output); err != nil {
		return orderdomain.DeliveryEstimate{}, err
	}
	return output.Estimate, nil
}

func (c *Client) call(ctx context.Context, name string, input, output any) error {
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		return fmt.Errorf("call shipping MCP tool %q: %w", name, err)
	}
	if result.IsError {
		return fmt.Errorf("shipping MCP tool %q failed: %s", name, toolErrorText(result))
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("marshal shipping MCP tool %q output: %w", name, err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode shipping MCP tool %q output: %w", name, err)
	}
	return nil
}

func toolErrorText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return "unknown tool error"
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
