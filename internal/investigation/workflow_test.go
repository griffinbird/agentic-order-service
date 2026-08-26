package investigation_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/griffinbird/agentic-order-service/internal/investigation"
	"github.com/griffinbird/agentic-order-service/internal/orderagent"
	"github.com/griffinbird/agentic-order-service/internal/orderdata"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/griffinbird/agentic-order-service/internal/shippingmcp"
	"github.com/microsoft/agent-framework-go/workflow"
	"github.com/microsoft/agent-framework-go/workflow/inproc"
)

type fakeReasoner struct{}

func (fakeReasoner) Recommend(
	_ context.Context,
	evidence orderdomain.InvestigationEvidence,
	stream orderagent.StreamFunc,
) (orderdomain.Recommendation, error) {
	if stream != nil {
		stream(`{"summary":"Transfer from Melbourne to Sydney"}`)
	}
	var estimate orderdomain.DeliveryEstimate
	for _, candidate := range evidence.DeliveryEstimates {
		if candidate.Origin == orderdomain.SydneyWarehouse {
			estimate = candidate
		}
	}
	item := evidence.Order.Items[0]
	return orderdomain.Recommendation{
		Summary:        "Transfer from Melbourne to Sydney.",
		ProposedAction: "transfer_fulfilment",
		Rationale: []string{
			"Payment is captured.",
			"Melbourne has no stock and Sydney has stock.",
		},
		TransferProposal: &orderdomain.TransferProposal{
			OrderID:              evidence.Order.ID,
			FromWarehouse:        evidence.Order.Warehouse,
			ToWarehouse:          orderdomain.SydneyWarehouse,
			SKU:                  item.SKU,
			Quantity:             item.Quantity,
			ExpectedOrderVersion: evidence.Order.Version,
			AdditionalDays:       estimate.AdditionalDays,
			AdditionalCostCents:  estimate.AdditionalCostCents,
			Currency:             estimate.Currency,
		},
	}, nil
}

func TestInvestigationWorkflowAssemblesEvidence(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	shipping := newShippingClient(t)
	wf, err := investigation.BuildInvestigationWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: shipping,
		Reasoner: fakeReasoner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runInvestigation(t, wf)

	if result.Evidence.Order.ID != orderdomain.DemoOrderID {
		t.Fatalf("unexpected order: %+v", result.Evidence.Order)
	}
	if result.Evidence.Payment.Status != orderdomain.PaymentCaptured {
		t.Fatalf("unexpected payment: %+v", result.Evidence.Payment)
	}
	gotInventory := make(map[string]int)
	for _, inventory := range result.Evidence.Inventory {
		gotInventory[inventory.Warehouse] = inventory.Available
	}
	if gotInventory[orderdomain.MelbourneWarehouse] != 0 || gotInventory[orderdomain.SydneyWarehouse] != 14 {
		t.Fatalf("unexpected inventory: %v", gotInventory)
	}
	if result.Evidence.ShippingStatus != nil {
		t.Fatalf("shipping status should not be queried without a shipment: %+v", result.Evidence.ShippingStatus)
	}
	if err := investigation.ValidateRecommendation(result); err != nil {
		t.Fatalf("valid recommendation rejected: %v", err)
	}
}

func TestWorkflowFanOutUsesConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	gate := &parallelGate{
		entered: make(chan string, 8),
		release: make(chan struct{}),
	}
	baseServices := orderdata.NewDeterministicServices()
	services := &trackedServices{MemoryServices: baseServices, gate: gate}
	shipping := &trackedShipping{ShippingReader: newShippingClient(t), gate: gate}
	wf, err := investigation.BuildInvestigationWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: shipping,
		Reasoner: fakeReasoner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := collectInvestigation(context.Background(), wf)
		result <- err
	}()

	var entered []string
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(entered) < 4 {
		select {
		case name := <-gate.entered:
			entered = append(entered, name)
		case <-timer.C:
			t.Fatalf("fan-out did not overlap; entered=%v", entered)
		}
	}
	close(gate.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	slices.Sort(entered)
	want := []string{"fulfilment", "inventory", "payment", "shipping"}
	if !slices.Equal(entered, want) {
		t.Fatalf("first concurrent calls = %v, want %v", entered, want)
	}
}

func TestValidateRecommendationRejectsInventedProposal(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	shipping := newShippingClient(t)
	wf, err := investigation.BuildInvestigationWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: shipping,
		Reasoner: fakeReasoner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runInvestigation(t, wf)
	result.Recommendation.TransferProposal.ToWarehouse = "BNE"
	if err := investigation.ValidateRecommendation(result); err == nil {
		t.Fatal("expected invented target warehouse to be rejected")
	}
}

func runInvestigation(t *testing.T, wf *workflow.Workflow) investigation.InvestigationResult {
	t.Helper()
	result, err := collectInvestigation(context.Background(), wf)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func collectInvestigation(ctx context.Context, wf *workflow.Workflow) (investigation.InvestigationResult, error) {
	run, err := inproc.Default.RunStreaming(ctx, wf, orderdomain.DemoOrderID)
	if err != nil {
		return investigation.InvestigationResult{}, err
	}
	defer run.Close(ctx)
	var eventTypes []string
	for event, eventErr := range run.WatchStream(ctx) {
		if eventErr != nil {
			return investigation.InvestigationResult{}, eventErr
		}
		eventTypes = append(eventTypes, fmt.Sprintf("%T", event))
		switch value := event.(type) {
		case workflow.OutputEvent:
			result, ok := value.Output.(investigation.InvestigationResult)
			if !ok {
				return investigation.InvestigationResult{}, fmt.Errorf("unexpected output type %T", value.Output)
			}
			return result, nil
		case workflow.ErrorEvent:
			return investigation.InvestigationResult{}, value.Error
		case workflow.ExecutorFailedEvent:
			return investigation.InvestigationResult{}, value.Error
		}
	}
	return investigation.InvestigationResult{}, fmt.Errorf("workflow completed without output; events=%v", eventTypes)
}

func newShippingClient(t *testing.T) *shippingmcp.Client {
	t.Helper()
	server := httptest.NewServer(shippingmcp.NewHTTPHandler(shippingmcp.NewServer(shippingmcp.NewService())))
	t.Cleanup(server.Close)
	client, err := shippingmcp.Connect(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close shipping client: %v", err)
		}
	})
	return client
}

type parallelGate struct {
	entered chan string
	release chan struct{}
}

func (g *parallelGate) wait(name string) {
	g.entered <- name
	<-g.release
}

type trackedServices struct {
	*orderdata.MemoryServices
	gate *parallelGate
}

func (s *trackedServices) GetPayment(ctx context.Context, paymentID string) (orderdomain.Payment, error) {
	s.gate.wait("payment")
	return s.MemoryServices.GetPayment(ctx, paymentID)
}

func (s *trackedServices) GetInventory(ctx context.Context, sku, warehouse string) (orderdomain.Inventory, error) {
	s.gate.wait("inventory")
	return s.MemoryServices.GetInventory(ctx, sku, warehouse)
}

func (s *trackedServices) GetFulfilment(ctx context.Context, orderID string) (orderdomain.Fulfilment, error) {
	s.gate.wait("fulfilment")
	return s.MemoryServices.GetFulfilment(ctx, orderID)
}

type trackedShipping struct {
	investigation.ShippingReader
	gate *parallelGate
}

func (s *trackedShipping) EstimateDelivery(ctx context.Context, origin, destination string) (orderdomain.DeliveryEstimate, error) {
	s.gate.wait("shipping")
	return s.ShippingReader.EstimateDelivery(ctx, origin, destination)
}
