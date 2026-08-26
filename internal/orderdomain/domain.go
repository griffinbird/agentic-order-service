package orderdomain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	DemoOrderID             = "58372"
	MelbourneWarehouse      = "MEL"
	SydneyWarehouse         = "SYD"
	TransferPermission      = "order.fulfilment.transfer"
	DefaultDestination      = "customer"
	DefaultIdempotencyKey   = "transfer-58372-mel-syd"
	DefaultCheckpointFolder = ".order-demo"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidArgument = errors.New("invalid argument")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrApprovalNeeded  = errors.New("approval required")
	ErrConflict        = errors.New("conflict")
)

type OrderStatus string

const (
	OrderAwaitingFulfilment OrderStatus = "awaiting_fulfilment"
	OrderReadyToShip        OrderStatus = "ready_to_ship"
)

type PaymentStatus string

const (
	PaymentCaptured PaymentStatus = "captured"
)

type FulfilmentState string

const (
	FulfilmentBlocked FulfilmentState = "blocked_waiting_for_stock"
	FulfilmentReady   FulfilmentState = "ready"
)

type OrderItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type Order struct {
	ID         string      `json:"id"`
	Status     OrderStatus `json:"status"`
	PaymentID  string      `json:"paymentId"`
	Warehouse  string      `json:"warehouse"`
	Items      []OrderItem `json:"items"`
	ShipmentID string      `json:"shipmentId,omitempty"`
	Version    int         `json:"version"`
}

type Payment struct {
	ID          string        `json:"id"`
	OrderID     string        `json:"orderId"`
	Status      PaymentStatus `json:"status"`
	AmountCents int           `json:"amountCents"`
	Currency    string        `json:"currency"`
}

type Inventory struct {
	SKU       string `json:"sku"`
	Warehouse string `json:"warehouse"`
	Available int    `json:"available"`
}

type Fulfilment struct {
	OrderID   string          `json:"orderId"`
	Warehouse string          `json:"warehouse"`
	State     FulfilmentState `json:"state"`
}

type ShippingStatus struct {
	ShipmentID string `json:"shipmentId"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
}

type DeliveryEstimate struct {
	Origin                string `json:"origin"`
	Destination           string `json:"destination"`
	EstimatedDeliveryDate string `json:"estimatedDeliveryDate"`
	AdditionalDays        int    `json:"additionalDays"`
	AdditionalCostCents   int    `json:"additionalCostCents"`
	Currency              string `json:"currency"`
}

type InvestigationEvidence struct {
	Order             Order              `json:"order"`
	Payment           Payment            `json:"payment"`
	Inventory         []Inventory        `json:"inventory"`
	Fulfilment        Fulfilment         `json:"fulfilment"`
	ShippingStatus    *ShippingStatus    `json:"shippingStatus,omitempty"`
	DeliveryEstimates []DeliveryEstimate `json:"deliveryEstimates"`
}

type TransferProposal struct {
	OrderID              string `json:"orderId"`
	FromWarehouse        string `json:"fromWarehouse"`
	ToWarehouse          string `json:"toWarehouse"`
	SKU                  string `json:"sku"`
	Quantity             int    `json:"quantity"`
	ExpectedOrderVersion int    `json:"expectedOrderVersion"`
	AdditionalDays       int    `json:"additionalDays"`
	AdditionalCostCents  int    `json:"additionalCostCents"`
	Currency             string `json:"currency"`
}

type Recommendation struct {
	Summary          string            `json:"summary" jsonschema:"A concise evidence-grounded recommendation"`
	ProposedAction   string            `json:"proposedAction" jsonschema:"The proposed action, or no_action"`
	Rationale        []string          `json:"rationale" jsonschema:"Short reasons tied only to supplied evidence"`
	TransferProposal *TransferProposal `json:"transferProposal,omitempty" jsonschema:"Required only when proposing transfer_fulfilment"`
}

func (p TransferProposal) Digest() (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal transfer proposal: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type ApprovalDecision struct {
	Approved       bool     `json:"approved"`
	Actor          string   `json:"actor"`
	Permissions    []string `json:"permissions"`
	ProposalDigest string   `json:"proposalDigest"`
	RequestID      string   `json:"requestId"`
}

func (a ApprovalDecision) HasPermission(permission string) bool {
	return slices.Contains(a.Permissions, permission)
}

type TransferCommand struct {
	Proposal       TransferProposal `json:"proposal"`
	Approval       ApprovalDecision `json:"approval"`
	IdempotencyKey string           `json:"idempotencyKey"`
}

type TransferResult struct {
	Order          Order  `json:"order"`
	IdempotencyKey string `json:"idempotencyKey"`
	AlreadyApplied bool   `json:"alreadyApplied"`
}

type OrderReader interface {
	GetOrder(context.Context, string) (Order, error)
}

type PaymentReader interface {
	GetPayment(context.Context, string) (Payment, error)
}

type InventoryReader interface {
	GetInventory(context.Context, string, string) (Inventory, error)
}

type FulfilmentReader interface {
	GetFulfilment(context.Context, string) (Fulfilment, error)
}

type FulfilmentTransfer interface {
	TransferFulfilment(context.Context, TransferCommand) (TransferResult, error)
}

type Services interface {
	OrderReader
	PaymentReader
	InventoryReader
	FulfilmentReader
	FulfilmentTransfer
}

func ValidateID(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidArgument, kind)
	}
	return nil
}
