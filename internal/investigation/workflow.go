package investigation

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/griffinbird/agentic-order-service/internal/orderagent"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/microsoft/agent-framework-go/workflow"
	workflowobservability "github.com/microsoft/agent-framework-go/workflow/observability"
)

const ApprovalPortID = "ApproveFulfilmentTransfer"

const (
	approvalStateKey   = "prepared-transfer"
	approvalStateScope = "order-resolution"
)

type ShippingReader interface {
	GetShippingStatus(context.Context, string) (orderdomain.ShippingStatus, error)
	EstimateDelivery(context.Context, string, string) (orderdomain.DeliveryEstimate, error)
}

type Dependencies struct {
	Services orderdomain.Services
	Shipping ShippingReader
	Reasoner orderagent.Reasoner
	Tracer   workflowobservability.Tracer
}

type AgentUpdateEvent struct {
	Text string
}

func (e AgentUpdateEvent) Data() any {
	return e.Text
}

type InvestigationResult struct {
	Evidence       orderdomain.InvestigationEvidence `json:"evidence"`
	Recommendation orderdomain.Recommendation        `json:"recommendation"`
}

type ApprovalRequest struct {
	RecommendationSummary string                       `json:"recommendationSummary"`
	Proposal              orderdomain.TransferProposal `json:"proposal"`
}

type ApprovalResponse struct {
	Decision orderdomain.ApprovalDecision `json:"decision"`
}

type ResolutionResult struct {
	Status   string                      `json:"status"`
	Transfer *orderdomain.TransferResult `json:"transfer,omitempty"`
}

type loadedOrder struct {
	Order orderdomain.Order
}

type evidencePart struct {
	Kind              string
	Order             orderdomain.Order
	Payment           *orderdomain.Payment
	Inventory         []orderdomain.Inventory
	Fulfilment        *orderdomain.Fulfilment
	ShippingStatus    *orderdomain.ShippingStatus
	DeliveryEstimates []orderdomain.DeliveryEstimate
}

type graphBindings struct {
	load      workflow.ExecutorBinding
	checks    []workflow.ExecutorBinding
	aggregate workflow.ExecutorBinding
	reason    workflow.ExecutorBinding
}

func BuildInvestigationWorkflow(deps Dependencies) (*workflow.Workflow, error) {
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}
	graph := newGraphBindings(deps)
	builder := workflow.NewBuilder(graph.load).
		AddFanOutEdge(graph.load, graph.checks).
		AddFanInBarrierEdge(graph.checks, graph.aggregate).
		AddEdge(graph.aggregate, graph.reason).
		WithOutputFrom(graph.reason)
	if deps.Tracer != nil {
		builder.WithTelemetry(deps.Tracer, workflow.TelemetryOptions{EnableSensitiveData: false})
	}
	return builder.Build()
}

func BuildResolutionWorkflow(deps Dependencies) (*workflow.Workflow, error) {
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}
	graph := newGraphBindings(deps)
	prepare := workflow.NewExecutor("PrepareApproval", func(ctx *workflow.Context, result InvestigationResult) (ApprovalRequest, error) {
		if err := ValidateRecommendation(result); err != nil {
			return ApprovalRequest{}, err
		}
		request := ApprovalRequest{
			RecommendationSummary: result.Recommendation.Summary,
			Proposal:              *result.Recommendation.TransferProposal,
		}
		if err := ctx.QueueStateUpdate(approvalStateKey, approvalStateScope, request); err != nil {
			return ApprovalRequest{}, fmt.Errorf("persist prepared transfer: %w", err)
		}
		return request, nil
	}).Bind()
	port := workflow.RequestPort{
		ID:       ApprovalPortID,
		Request:  reflect.TypeFor[ApprovalRequest](),
		Response: reflect.TypeFor[ApprovalResponse](),
	}
	approval := newApprovalBinding(port)
	transfer := workflow.NewExecutor("TransferFulfilment", func(ctx *workflow.Context, response ApprovalResponse) (ResolutionResult, error) {
		if !response.Decision.Approved {
			return ResolutionResult{Status: "rejected"}, nil
		}
		state, err := ctx.ReadState(approvalStateKey, approvalStateScope)
		if err != nil {
			return ResolutionResult{}, fmt.Errorf("read prepared transfer: %w", err)
		}
		request, ok := state.(ApprovalRequest)
		if !ok {
			return ResolutionResult{}, fmt.Errorf("prepared transfer has unexpected type %T", state)
		}
		digest, err := request.Proposal.Digest()
		if err != nil {
			return ResolutionResult{}, err
		}
		if response.Decision.ProposalDigest != digest {
			return ResolutionResult{}, fmt.Errorf("%w: approval does not match the prepared transfer", orderdomain.ErrApprovalNeeded)
		}
		result, err := deps.Services.TransferFulfilment(ctx, orderdomain.TransferCommand{
			Proposal:       request.Proposal,
			Approval:       response.Decision,
			IdempotencyKey: orderdomain.DefaultIdempotencyKey,
		})
		if err != nil {
			return ResolutionResult{}, err
		}
		return ResolutionResult{Status: "transferred", Transfer: &result}, nil
	}).Bind()

	builder := workflow.NewBuilder(graph.load).
		AddFanOutEdge(graph.load, graph.checks).
		AddFanInBarrierEdge(graph.checks, graph.aggregate).
		AddEdge(graph.aggregate, graph.reason).
		AddEdge(graph.reason, prepare).
		AddEdge(prepare, approval).
		AddEdge(approval, transfer).
		WithOutputFrom(transfer)
	if deps.Tracer != nil {
		builder.WithTelemetry(deps.Tracer, workflow.TelemetryOptions{EnableSensitiveData: false})
	}
	return builder.Build()
}

func newApprovalBinding(port workflow.RequestPort) workflow.ExecutorBinding {
	const executorID = "HumanApproval"
	return workflow.ExecutorBinding{
		ID:               executorID,
		ImplementationID: "order-demo.HumanApproval",
		NewExecutorFunc: func(string) (*workflow.Executor, error) {
			return &workflow.Executor{
				ID: executorID,
				DisableAutoSendMessageHandlerResultObject: true,
				DisableAutoYieldOutputHandlerResultObject: true,
				ConfigureProtocol: func(builder *workflow.ProtocolBuilder) (*workflow.ProtocolBuilder, error) {
					builder.SendsMessageType(reflect.TypeFor[ApprovalResponse]())
					builder.RouteBuilder.
						AddHandlerRaw(reflect.TypeFor[ApprovalRequest](), nil, func(ctx *workflow.Context, message any) (any, error) {
							request, err := workflow.NewExternalRequest("", port, message)
							if err != nil {
								return nil, err
							}
							return nil, ctx.PostRequest(request)
						}).
						AddHandlerRaw(reflect.TypeFor[*workflow.ExternalResponse](), nil, func(ctx *workflow.Context, message any) (any, error) {
							response := message.(*workflow.ExternalResponse)
							if response.PortInfo.PortID != port.ID {
								return nil, fmt.Errorf("unexpected approval response port %q", response.PortInfo.PortID)
							}
							value, ok := response.Data.As(port.Response)
							if !ok {
								return nil, fmt.Errorf("unexpected approval response type %T", response.Data)
							}
							return nil, ctx.SendMessage("", value.(ApprovalResponse))
						})
					return builder, nil
				},
			}, nil
		},
	}
}

func newGraphBindings(deps Dependencies) graphBindings {
	load := workflow.NewExecutor("LoadOrder", func(ctx *workflow.Context, orderID string) (loadedOrder, error) {
		order, err := deps.Services.GetOrder(ctx, orderID)
		if err != nil {
			return loadedOrder{}, fmt.Errorf("load order: %w", err)
		}
		return loadedOrder{Order: order}, nil
	}).Bind()
	payment := workflow.NewExecutor("CheckPayment", func(ctx *workflow.Context, input loadedOrder) (evidencePart, error) {
		value, err := deps.Services.GetPayment(ctx, input.Order.PaymentID)
		if err != nil {
			return evidencePart{}, fmt.Errorf("check payment: %w", err)
		}
		return evidencePart{Kind: "payment", Order: input.Order, Payment: &value}, nil
	}).Bind()
	inventory := workflow.NewExecutor("CheckInventory", func(ctx *workflow.Context, input loadedOrder) (evidencePart, error) {
		if len(input.Order.Items) == 0 {
			return evidencePart{}, fmt.Errorf("check inventory: order has no items")
		}
		item := input.Order.Items[0]
		warehouses := []string{input.Order.Warehouse, orderdomain.SydneyWarehouse}
		values := make([]orderdomain.Inventory, 0, len(warehouses))
		for _, warehouse := range warehouses {
			if slices.ContainsFunc(values, func(value orderdomain.Inventory) bool {
				return value.Warehouse == warehouse
			}) {
				continue
			}
			value, err := deps.Services.GetInventory(ctx, item.SKU, warehouse)
			if err != nil {
				return evidencePart{}, fmt.Errorf("check inventory at %s: %w", warehouse, err)
			}
			values = append(values, value)
		}
		return evidencePart{Kind: "inventory", Order: input.Order, Inventory: values}, nil
	}).Bind()
	fulfilment := workflow.NewExecutor("CheckFulfilment", func(ctx *workflow.Context, input loadedOrder) (evidencePart, error) {
		value, err := deps.Services.GetFulfilment(ctx, input.Order.ID)
		if err != nil {
			return evidencePart{}, fmt.Errorf("check fulfilment: %w", err)
		}
		return evidencePart{Kind: "fulfilment", Order: input.Order, Fulfilment: &value}, nil
	}).Bind()
	shipping := workflow.NewExecutor("CheckShipping", func(ctx *workflow.Context, input loadedOrder) (evidencePart, error) {
		part := evidencePart{Kind: "shipping", Order: input.Order}
		if input.Order.ShipmentID != "" {
			status, err := deps.Shipping.GetShippingStatus(ctx, input.Order.ShipmentID)
			if err != nil {
				return evidencePart{}, fmt.Errorf("check shipping status: %w", err)
			}
			part.ShippingStatus = &status
		}
		estimate, err := deps.Shipping.EstimateDelivery(ctx, orderdomain.SydneyWarehouse, orderdomain.DefaultDestination)
		if err != nil {
			return evidencePart{}, fmt.Errorf("estimate Sydney delivery: %w", err)
		}
		part.DeliveryEstimates = []orderdomain.DeliveryEstimate{estimate}
		return part, nil
	}).Bind()
	aggregate := newEvidenceAggregator().Bind()
	reason := workflow.NewExecutor("ReasonOverEvidence", func(ctx *workflow.Context, evidence orderdomain.InvestigationEvidence) (InvestigationResult, error) {
		recommendation, err := deps.Reasoner.Recommend(ctx, evidence, func(text string) {
			_ = ctx.AddEvent(AgentUpdateEvent{Text: text})
		})
		if err != nil {
			return InvestigationResult{}, fmt.Errorf("reason over evidence: %w", err)
		}
		return InvestigationResult{Evidence: evidence, Recommendation: recommendation}, nil
	}).Bind()
	return graphBindings{
		load: load,
		checks: []workflow.ExecutorBinding{
			payment, inventory, fulfilment, shipping,
		},
		aggregate: aggregate,
		reason:    reason,
	}
}

func newEvidenceAggregator() *workflow.Executor {
	var parts []evidencePart
	return workflow.NewExecutor("AssembleEvidence", func(part evidencePart) {
		parts = append(parts, part)
	}).Extend(&workflow.Executor{
		ConfigureProtocol: func(builder *workflow.ProtocolBuilder) (*workflow.ProtocolBuilder, error) {
			builder.SendsMessageType(reflect.TypeFor[orderdomain.InvestigationEvidence]())
			return builder, nil
		},
		OnMessageDeliveryFinishedFunc: func(ctx *workflow.Context) error {
			evidence, err := assembleEvidence(parts)
			parts = nil
			if err != nil {
				return err
			}
			return ctx.SendMessage("", evidence)
		},
	})
}

func assembleEvidence(parts []evidencePart) (orderdomain.InvestigationEvidence, error) {
	var evidence orderdomain.InvestigationEvidence
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		if seen[part.Kind] {
			return orderdomain.InvestigationEvidence{}, fmt.Errorf("duplicate evidence part %q", part.Kind)
		}
		seen[part.Kind] = true
		if evidence.Order.ID == "" {
			evidence.Order = part.Order
		} else if evidence.Order.ID != part.Order.ID || evidence.Order.Version != part.Order.Version {
			return orderdomain.InvestigationEvidence{}, fmt.Errorf("evidence parts refer to different order versions")
		}
		switch part.Kind {
		case "payment":
			if part.Payment == nil {
				return orderdomain.InvestigationEvidence{}, fmt.Errorf("payment evidence is empty")
			}
			evidence.Payment = *part.Payment
		case "inventory":
			evidence.Inventory = append([]orderdomain.Inventory(nil), part.Inventory...)
		case "fulfilment":
			if part.Fulfilment == nil {
				return orderdomain.InvestigationEvidence{}, fmt.Errorf("fulfilment evidence is empty")
			}
			evidence.Fulfilment = *part.Fulfilment
		case "shipping":
			evidence.ShippingStatus = part.ShippingStatus
			evidence.DeliveryEstimates = append([]orderdomain.DeliveryEstimate(nil), part.DeliveryEstimates...)
		default:
			return orderdomain.InvestigationEvidence{}, fmt.Errorf("unknown evidence part %q", part.Kind)
		}
	}
	for _, required := range []string{"payment", "inventory", "fulfilment", "shipping"} {
		if !seen[required] {
			return orderdomain.InvestigationEvidence{}, fmt.Errorf("missing evidence part %q", required)
		}
	}
	slices.SortFunc(evidence.Inventory, func(a, b orderdomain.Inventory) int {
		return strings.Compare(a.Warehouse, b.Warehouse)
	})
	slices.SortFunc(evidence.DeliveryEstimates, func(a, b orderdomain.DeliveryEstimate) int {
		return strings.Compare(a.Origin, b.Origin)
	})
	return evidence, nil
}

func ValidateRecommendation(result InvestigationResult) error {
	recommendation := result.Recommendation
	if recommendation.ProposedAction != "transfer_fulfilment" || recommendation.TransferProposal == nil {
		return fmt.Errorf("%w: recommendation does not contain a fulfilment transfer", orderdomain.ErrConflict)
	}
	evidence := result.Evidence
	proposal := *recommendation.TransferProposal
	if len(evidence.Order.Items) != 1 {
		return fmt.Errorf("%w: expected one order item", orderdomain.ErrConflict)
	}
	item := evidence.Order.Items[0]
	if evidence.Order.Status != orderdomain.OrderAwaitingFulfilment ||
		evidence.Payment.Status != orderdomain.PaymentCaptured ||
		evidence.Fulfilment.State != orderdomain.FulfilmentBlocked {
		return fmt.Errorf("%w: evidence does not support transfer", orderdomain.ErrConflict)
	}
	var targetInventory *orderdomain.Inventory
	for i := range evidence.Inventory {
		inventory := &evidence.Inventory[i]
		if inventory.Warehouse == proposal.ToWarehouse && inventory.SKU == item.SKU {
			targetInventory = inventory
			break
		}
	}
	var estimate *orderdomain.DeliveryEstimate
	for i := range evidence.DeliveryEstimates {
		candidate := &evidence.DeliveryEstimates[i]
		if candidate.Origin == proposal.ToWarehouse {
			estimate = candidate
			break
		}
	}
	if targetInventory == nil || targetInventory.Available < item.Quantity || estimate == nil {
		return fmt.Errorf("%w: target warehouse evidence is incomplete", orderdomain.ErrConflict)
	}
	if proposal.OrderID != evidence.Order.ID ||
		proposal.FromWarehouse != evidence.Order.Warehouse ||
		proposal.ToWarehouse == evidence.Order.Warehouse ||
		proposal.SKU != item.SKU ||
		proposal.Quantity != item.Quantity ||
		proposal.ExpectedOrderVersion != evidence.Order.Version ||
		proposal.AdditionalDays != estimate.AdditionalDays ||
		proposal.AdditionalCostCents != estimate.AdditionalCostCents ||
		proposal.Currency != estimate.Currency {
		return fmt.Errorf("%w: transfer proposal does not match evidence", orderdomain.ErrConflict)
	}
	return nil
}

func validateDependencies(deps Dependencies) error {
	if deps.Services == nil {
		return fmt.Errorf("order services are required")
	}
	if deps.Shipping == nil {
		return fmt.Errorf("shipping MCP client is required")
	}
	if deps.Reasoner == nil {
		return fmt.Errorf("agent reasoner is required")
	}
	return nil
}
