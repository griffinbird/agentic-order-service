package orderdata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/griffinbird/agentic-order-service/internal/orderdata"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
)

func TestDeterministicScenario(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	ctx := context.Background()

	order, err := services.GetOrder(ctx, orderdomain.DemoOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if order.Warehouse != orderdomain.MelbourneWarehouse || order.Status != orderdomain.OrderAwaitingFulfilment {
		t.Fatalf("unexpected order: %+v", order)
	}
	payment, err := services.GetPayment(ctx, order.PaymentID)
	if err != nil {
		t.Fatal(err)
	}
	if payment.Status != orderdomain.PaymentCaptured {
		t.Fatalf("unexpected payment: %+v", payment)
	}
	melbourne, err := services.GetInventory(ctx, "SKU-441", orderdomain.MelbourneWarehouse)
	if err != nil {
		t.Fatal(err)
	}
	sydney, err := services.GetInventory(ctx, "SKU-441", orderdomain.SydneyWarehouse)
	if err != nil {
		t.Fatal(err)
	}
	if melbourne.Available != 0 || sydney.Available != 14 {
		t.Fatalf("unexpected inventory: Melbourne=%d Sydney=%d", melbourne.Available, sydney.Available)
	}
}

func TestGetOrderHonorsCancellation(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := services.GetOrder(ctx, orderdomain.DemoOrderID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestTransferRequiresMatchingAuthorizedApprovalAndIsIdempotent(t *testing.T) {
	t.Parallel()
	services := orderdata.NewDeterministicServices()
	proposal := demoProposal()
	digest, err := proposal.Digest()
	if err != nil {
		t.Fatal(err)
	}
	command := orderdomain.TransferCommand{
		Proposal: proposal,
		Approval: orderdomain.ApprovalDecision{
			Approved:       true,
			Actor:          "operator@example.com",
			Permissions:    []string{orderdomain.TransferPermission},
			ProposalDigest: digest,
			RequestID:      "request-1",
		},
		IdempotencyKey: orderdomain.DefaultIdempotencyKey,
	}

	first, err := services.TransferFulfilment(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.AlreadyApplied || first.Order.Warehouse != orderdomain.SydneyWarehouse {
		t.Fatalf("unexpected first result: %+v", first)
	}
	second, err := services.TransferFulfilment(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyApplied || second.Order.Version != first.Order.Version {
		t.Fatalf("unexpected repeated result: %+v", second)
	}
}

func TestTransferRejectsUnauthorizedOrStaleCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*orderdomain.TransferCommand)
		wantErr error
	}{
		{
			name: "missing permission",
			mutate: func(command *orderdomain.TransferCommand) {
				command.Approval.Permissions = nil
			},
			wantErr: orderdomain.ErrUnauthorized,
		},
		{
			name: "wrong proposal digest",
			mutate: func(command *orderdomain.TransferCommand) {
				command.Approval.ProposalDigest = "wrong"
			},
			wantErr: orderdomain.ErrApprovalNeeded,
		},
		{
			name: "stale version",
			mutate: func(command *orderdomain.TransferCommand) {
				command.Proposal.ExpectedOrderVersion--
				command.Approval.ProposalDigest, _ = command.Proposal.Digest()
			},
			wantErr: orderdomain.ErrConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			services := orderdata.NewDeterministicServices()
			proposal := demoProposal()
			digest, err := proposal.Digest()
			if err != nil {
				t.Fatal(err)
			}
			command := orderdomain.TransferCommand{
				Proposal: proposal,
				Approval: orderdomain.ApprovalDecision{
					Approved:       true,
					Actor:          "operator@example.com",
					Permissions:    []string{orderdomain.TransferPermission},
					ProposalDigest: digest,
					RequestID:      "request-1",
				},
				IdempotencyKey: orderdomain.DefaultIdempotencyKey,
			}
			tt.mutate(&command)
			_, err = services.TransferFulfilment(context.Background(), command)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func demoProposal() orderdomain.TransferProposal {
	return orderdomain.TransferProposal{
		OrderID:              orderdomain.DemoOrderID,
		FromWarehouse:        orderdomain.MelbourneWarehouse,
		ToWarehouse:          orderdomain.SydneyWarehouse,
		SKU:                  "SKU-441",
		Quantity:             1,
		ExpectedOrderVersion: 7,
		AdditionalDays:       1,
		AdditionalCostCents:  840,
		Currency:             "AUD",
	}
}
