package resolution_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/investigation"
	"github.com/griffinbird/agentic-order-service/internal/orderagent"
	"github.com/griffinbird/agentic-order-service/internal/orderdata"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/griffinbird/agentic-order-service/internal/resolution"
	"github.com/microsoft/agent-framework-go/workflow"
)

type fixedReasoner struct{}

func (fixedReasoner) Recommend(
	_ context.Context,
	evidence orderdomain.InvestigationEvidence,
	_ orderagent.StreamFunc,
) (orderdomain.Recommendation, error) {
	item := evidence.Order.Items[0]
	estimate := evidence.DeliveryEstimates[0]
	return orderdomain.Recommendation{
		Summary:        "Transfer fulfilment to Sydney.",
		ProposedAction: "transfer_fulfilment",
		Rationale:      []string{"Payment captured.", "Sydney has inventory."},
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

type fixedShipping struct{}

func (fixedShipping) GetShippingStatus(context.Context, string) (orderdomain.ShippingStatus, error) {
	return orderdomain.ShippingStatus{}, nil
}

func (fixedShipping) EstimateDelivery(_ context.Context, origin, destination string) (orderdomain.DeliveryEstimate, error) {
	return orderdomain.DeliveryEstimate{
		Origin:                origin,
		Destination:           destination,
		EstimatedDeliveryDate: "2026-08-29",
		AdditionalDays:        1,
		AdditionalCostCents:   840,
		Currency:              "AUD",
	}, nil
}

type unavailableReasoner struct {
	calls *int
}

func (r unavailableReasoner) Recommend(
	context.Context,
	orderdomain.InvestigationEvidence,
	orderagent.StreamFunc,
) (orderdomain.Recommendation, error) {
	*r.calls++
	return orderdomain.Recommendation{}, fmt.Errorf("reasoner must not run after checkpoint restore")
}

type unavailableShipping struct {
	calls *int
}

func (s unavailableShipping) GetShippingStatus(context.Context, string) (orderdomain.ShippingStatus, error) {
	*s.calls++
	return orderdomain.ShippingStatus{}, fmt.Errorf("shipping must not run after checkpoint restore")
}

func (s unavailableShipping) EstimateDelivery(context.Context, string, string) (orderdomain.DeliveryEstimate, error) {
	*s.calls++
	return orderdomain.DeliveryEstimate{}, fmt.Errorf("shipping must not run after checkpoint restore")
}

func TestApprovalCheckpointSurvivesCloseAndResume(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	startWorkflow := newResolutionWorkflow(t, orderdata.NewDeterministicServices())
	paused, err := resolution.Start(
		context.Background(),
		startWorkflow,
		stateDir,
		orderdomain.DemoOrderID,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	if paused.RequestID == "" || paused.Checkpoint.CheckpointID == "" {
		t.Fatalf("incomplete paused run: %+v", paused)
	}

	var reasonerCalls, shippingCalls int
	resumeWorkflow := newResumeOnlyWorkflow(t, orderdata.NewDeterministicServices(), &reasonerCalls, &shippingCalls)
	result, err := resolution.Resume(
		context.Background(),
		resumeWorkflow,
		stateDir,
		true,
		"operator@example.com",
		[]string{orderdomain.TransferPermission},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "transferred" || result.Transfer == nil {
		t.Fatalf("unexpected resolution: %+v", result)
	}
	if result.Transfer.Order.Warehouse != orderdomain.SydneyWarehouse || result.Transfer.AlreadyApplied {
		t.Fatalf("unexpected transfer: %+v", result.Transfer)
	}
	if reasonerCalls != 0 || shippingCalls != 0 {
		t.Fatalf("resume reran completed dependencies: reasoner=%d shipping=%d", reasonerCalls, shippingCalls)
	}

	repeated, err := resolution.Resume(
		context.Background(),
		resumeWorkflow,
		stateDir,
		true,
		"operator@example.com",
		[]string{orderdomain.TransferPermission},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Transfer == nil || !repeated.Transfer.AlreadyApplied {
		t.Fatalf("repeated resume should return saved idempotent result: %+v", repeated)
	}
}

func TestApprovalCheckpointResumesAcrossProcesses(t *testing.T) {
	stateDir := t.TempDir()
	runResolutionHelper(t, "start", stateDir)
	runResolutionHelper(t, "resume", stateDir)
}

func TestResolutionProcessHelper(t *testing.T) {
	if os.Getenv("ORDER_DEMO_RESOLUTION_HELPER") != "1" {
		return
	}
	if len(os.Args) != 5 || os.Args[2] != "--" {
		t.Fatalf("unexpected helper arguments: %q", os.Args)
	}
	phase, stateDir := os.Args[3], os.Args[4]
	switch phase {
	case "start":
		if _, err := resolution.Start(
			context.Background(),
			newResolutionWorkflow(t, orderdata.NewDeterministicServices()),
			stateDir,
			orderdomain.DemoOrderID,
			nil,
		); err != nil {
			t.Fatal(err)
		}
	case "resume":
		result, err := resolution.Resume(
			context.Background(),
			newResumeOnlyWorkflow(t, orderdata.NewDeterministicServices(), new(int), new(int)),
			stateDir,
			true,
			"operator@example.com",
			[]string{orderdomain.TransferPermission},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "transferred" || result.Transfer == nil {
			t.Fatalf("unexpected resolution: %+v", result)
		}
	default:
		t.Fatalf("unknown helper phase %q", phase)
	}
}

func TestRejectedApprovalDoesNotTransfer(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	if _, err := resolution.Start(
		context.Background(),
		newResolutionWorkflow(t, orderdata.NewDeterministicServices()),
		stateDir,
		orderdomain.DemoOrderID,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	result, err := resolution.Resume(
		context.Background(),
		newResumeOnlyWorkflow(t, orderdata.NewDeterministicServices(), new(int), new(int)),
		stateDir,
		false,
		"operator@example.com",
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "rejected" || result.Transfer != nil {
		t.Fatalf("unexpected rejection result: %+v", result)
	}
}

func TestApprovedTransferStillRequiresPermission(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	if _, err := resolution.Start(
		context.Background(),
		newResolutionWorkflow(t, orderdata.NewDeterministicServices()),
		stateDir,
		orderdomain.DemoOrderID,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	_, err := resolution.Resume(
		context.Background(),
		newResumeOnlyWorkflow(t, orderdata.NewDeterministicServices(), new(int), new(int)),
		stateDir,
		true,
		"operator@example.com",
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("expected unauthorized transfer to fail")
	}
}

func newResolutionWorkflow(t *testing.T, services *orderdata.MemoryServices) *workflow.Workflow {
	t.Helper()
	wf, err := investigation.BuildResolutionWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: fixedShipping{},
		Reasoner: fixedReasoner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func newResumeOnlyWorkflow(
	t *testing.T,
	services *orderdata.MemoryServices,
	reasonerCalls *int,
	shippingCalls *int,
) *workflow.Workflow {
	t.Helper()
	wf, err := investigation.BuildResolutionWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: unavailableShipping{calls: shippingCalls},
		Reasoner: unavailableReasoner{calls: reasonerCalls},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func runResolutionHelper(t *testing.T, phase, stateDir string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestResolutionProcessHelper$", "--", phase, stateDir)
	command.Env = append(os.Environ(), "ORDER_DEMO_RESOLUTION_HELPER=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s helper failed: %v\n%s", phase, err, output)
	}
}
