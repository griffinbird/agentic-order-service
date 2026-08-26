package orderdata

import (
	"context"
	"fmt"
	"sync"

	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
)

type idempotentTransfer struct {
	proposalDigest string
	result         orderdomain.TransferResult
}

type MemoryServices struct {
	mu          sync.RWMutex
	orders      map[string]orderdomain.Order
	payments    map[string]orderdomain.Payment
	inventory   map[string]orderdomain.Inventory
	fulfilments map[string]orderdomain.Fulfilment
	transfers   map[string]idempotentTransfer
}

func NewDeterministicServices() *MemoryServices {
	order := orderdomain.Order{
		ID:        orderdomain.DemoOrderID,
		Status:    orderdomain.OrderAwaitingFulfilment,
		PaymentID: "PAY-58372",
		Warehouse: orderdomain.MelbourneWarehouse,
		Items: []orderdomain.OrderItem{
			{SKU: "SKU-441", Quantity: 1},
		},
		Version: 7,
	}
	return &MemoryServices{
		orders: map[string]orderdomain.Order{
			order.ID: order,
		},
		payments: map[string]orderdomain.Payment{
			order.PaymentID: {
				ID:          order.PaymentID,
				OrderID:     order.ID,
				Status:      orderdomain.PaymentCaptured,
				AmountCents: 12900,
				Currency:    "AUD",
			},
		},
		inventory: map[string]orderdomain.Inventory{
			inventoryKey("SKU-441", orderdomain.MelbourneWarehouse): {
				SKU: "SKU-441", Warehouse: orderdomain.MelbourneWarehouse, Available: 0,
			},
			inventoryKey("SKU-441", orderdomain.SydneyWarehouse): {
				SKU: "SKU-441", Warehouse: orderdomain.SydneyWarehouse, Available: 14,
			},
		},
		fulfilments: map[string]orderdomain.Fulfilment{
			order.ID: {
				OrderID: order.ID, Warehouse: orderdomain.MelbourneWarehouse, State: orderdomain.FulfilmentBlocked,
			},
		},
		transfers: make(map[string]idempotentTransfer),
	}
}

func (s *MemoryServices) GetOrder(ctx context.Context, orderID string) (orderdomain.Order, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.Order{}, err
	}
	if err := orderdomain.ValidateID("order ID", orderID); err != nil {
		return orderdomain.Order{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	order, ok := s.orders[orderID]
	if !ok {
		return orderdomain.Order{}, fmt.Errorf("order %q: %w", orderID, orderdomain.ErrNotFound)
	}
	order.Items = append([]orderdomain.OrderItem(nil), order.Items...)
	return order, nil
}

func (s *MemoryServices) GetPayment(ctx context.Context, paymentID string) (orderdomain.Payment, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.Payment{}, err
	}
	if err := orderdomain.ValidateID("payment ID", paymentID); err != nil {
		return orderdomain.Payment{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	payment, ok := s.payments[paymentID]
	if !ok {
		return orderdomain.Payment{}, fmt.Errorf("payment %q: %w", paymentID, orderdomain.ErrNotFound)
	}
	return payment, nil
}

func (s *MemoryServices) GetInventory(ctx context.Context, sku, warehouse string) (orderdomain.Inventory, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.Inventory{}, err
	}
	if err := orderdomain.ValidateID("SKU", sku); err != nil {
		return orderdomain.Inventory{}, err
	}
	if err := orderdomain.ValidateID("warehouse", warehouse); err != nil {
		return orderdomain.Inventory{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	inventory, ok := s.inventory[inventoryKey(sku, warehouse)]
	if !ok {
		return orderdomain.Inventory{}, fmt.Errorf("inventory for SKU %q at warehouse %q: %w", sku, warehouse, orderdomain.ErrNotFound)
	}
	return inventory, nil
}

func (s *MemoryServices) GetFulfilment(ctx context.Context, orderID string) (orderdomain.Fulfilment, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.Fulfilment{}, err
	}
	if err := orderdomain.ValidateID("order ID", orderID); err != nil {
		return orderdomain.Fulfilment{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	fulfilment, ok := s.fulfilments[orderID]
	if !ok {
		return orderdomain.Fulfilment{}, fmt.Errorf("fulfilment for order %q: %w", orderID, orderdomain.ErrNotFound)
	}
	return fulfilment, nil
}

func (s *MemoryServices) TransferFulfilment(ctx context.Context, command orderdomain.TransferCommand) (orderdomain.TransferResult, error) {
	if err := contextError(ctx); err != nil {
		return orderdomain.TransferResult{}, err
	}
	if err := validateTransferCommand(command); err != nil {
		return orderdomain.TransferResult{}, err
	}
	proposalDigest, err := command.Proposal.Digest()
	if err != nil {
		return orderdomain.TransferResult{}, err
	}
	if !command.Approval.Approved {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: transfer was not approved", orderdomain.ErrApprovalNeeded)
	}
	if command.Approval.ProposalDigest != proposalDigest {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: approval does not match transfer proposal", orderdomain.ErrApprovalNeeded)
	}
	if command.Approval.Actor == "" || !command.Approval.HasPermission(orderdomain.TransferPermission) {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: actor %q lacks %q", orderdomain.ErrUnauthorized, command.Approval.Actor, orderdomain.TransferPermission)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if prior, ok := s.transfers[command.IdempotencyKey]; ok {
		if prior.proposalDigest != proposalDigest {
			return orderdomain.TransferResult{}, fmt.Errorf("%w: idempotency key %q was used for a different transfer", orderdomain.ErrConflict, command.IdempotencyKey)
		}
		result := prior.result
		result.AlreadyApplied = true
		return result, nil
	}

	order, ok := s.orders[command.Proposal.OrderID]
	if !ok {
		return orderdomain.TransferResult{}, fmt.Errorf("order %q: %w", command.Proposal.OrderID, orderdomain.ErrNotFound)
	}
	if order.Status != orderdomain.OrderAwaitingFulfilment ||
		order.Warehouse != command.Proposal.FromWarehouse ||
		order.Version != command.Proposal.ExpectedOrderVersion {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: order state changed since proposal", orderdomain.ErrConflict)
	}
	if len(order.Items) != 1 ||
		order.Items[0].SKU != command.Proposal.SKU ||
		order.Items[0].Quantity != command.Proposal.Quantity {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: proposal items do not match order", orderdomain.ErrConflict)
	}
	payment := s.payments[order.PaymentID]
	if payment.Status != orderdomain.PaymentCaptured {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: payment is not captured", orderdomain.ErrConflict)
	}
	fulfilment, ok := s.fulfilments[order.ID]
	if !ok || fulfilment.State != orderdomain.FulfilmentBlocked || fulfilment.Warehouse != command.Proposal.FromWarehouse {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: fulfilment is not transferable", orderdomain.ErrConflict)
	}
	targetKey := inventoryKey(command.Proposal.SKU, command.Proposal.ToWarehouse)
	target, ok := s.inventory[targetKey]
	if !ok || target.Available < command.Proposal.Quantity {
		return orderdomain.TransferResult{}, fmt.Errorf("%w: insufficient target inventory", orderdomain.ErrConflict)
	}

	target.Available -= command.Proposal.Quantity
	s.inventory[targetKey] = target
	order.Warehouse = command.Proposal.ToWarehouse
	order.Status = orderdomain.OrderReadyToShip
	order.Version++
	s.orders[order.ID] = order
	fulfilment.Warehouse = command.Proposal.ToWarehouse
	fulfilment.State = orderdomain.FulfilmentReady
	s.fulfilments[order.ID] = fulfilment

	result := orderdomain.TransferResult{
		Order:          order,
		IdempotencyKey: command.IdempotencyKey,
	}
	s.transfers[command.IdempotencyKey] = idempotentTransfer{
		proposalDigest: proposalDigest,
		result:         result,
	}
	return result, nil
}

func validateTransferCommand(command orderdomain.TransferCommand) error {
	if err := orderdomain.ValidateID("idempotency key", command.IdempotencyKey); err != nil {
		return err
	}
	if err := orderdomain.ValidateID("order ID", command.Proposal.OrderID); err != nil {
		return err
	}
	if err := orderdomain.ValidateID("source warehouse", command.Proposal.FromWarehouse); err != nil {
		return err
	}
	if err := orderdomain.ValidateID("target warehouse", command.Proposal.ToWarehouse); err != nil {
		return err
	}
	if command.Proposal.FromWarehouse == command.Proposal.ToWarehouse {
		return fmt.Errorf("%w: source and target warehouse must differ", orderdomain.ErrInvalidArgument)
	}
	if err := orderdomain.ValidateID("SKU", command.Proposal.SKU); err != nil {
		return err
	}
	if command.Proposal.Quantity <= 0 {
		return fmt.Errorf("%w: quantity must be positive", orderdomain.ErrInvalidArgument)
	}
	if command.Proposal.ExpectedOrderVersion <= 0 {
		return fmt.Errorf("%w: expected order version must be positive", orderdomain.ErrInvalidArgument)
	}
	return nil
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func inventoryKey(sku, warehouse string) string {
	return sku + "\x00" + warehouse
}
