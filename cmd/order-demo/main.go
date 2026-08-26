package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/griffinbird/agentic-order-service/internal/investigation"
	"github.com/griffinbird/agentic-order-service/internal/orderagent"
	"github.com/griffinbird/agentic-order-service/internal/orderdata"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/griffinbird/agentic-order-service/internal/ordertool"
	"github.com/griffinbird/agentic-order-service/internal/resolution"
	"github.com/griffinbird/agentic-order-service/internal/shippingmcp"
	"github.com/griffinbird/agentic-order-service/internal/telemetry"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/workflow"
	"github.com/microsoft/agent-framework-go/workflow/inproc"
)

const defaultShippingEndpoint = "http://127.0.0.1:8081"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return fmt.Errorf("a command is required")
	}
	tracing, err := telemetry.SetupFromEnvironment()
	if err != nil {
		return err
	}
	defer func() {
		if shutdownErr := tracing.Shutdown(context.Background()); shutdownErr != nil {
			log.Printf("shutdown telemetry: %v", shutdownErr)
		}
	}()

	switch args[0] {
	case "domain":
		return runDomain(ctx)
	case "basic":
		return runAgent(ctx, tracing, false, args[1:])
	case "agent":
		return runAgent(ctx, tracing, true, args[1:])
	case "workflow":
		return runWorkflow(ctx, tracing, args[1:])
	case "resolve":
		return runResolve(ctx, tracing, args[1:])
	case "resume":
		return runResume(ctx, tracing, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runDomain(ctx context.Context) error {
	services := orderdata.NewDeterministicServices()
	order, err := services.GetOrder(ctx, orderdomain.DemoOrderID)
	if err != nil {
		return err
	}
	payment, err := services.GetPayment(ctx, order.PaymentID)
	if err != nil {
		return err
	}
	fulfilment, err := services.GetFulfilment(ctx, order.ID)
	if err != nil {
		return err
	}
	var inventory []orderdomain.Inventory
	for _, warehouse := range []string{orderdomain.MelbourneWarehouse, orderdomain.SydneyWarehouse} {
		value, inventoryErr := services.GetInventory(ctx, order.Items[0].SKU, warehouse)
		if inventoryErr != nil {
			return inventoryErr
		}
		inventory = append(inventory, value)
	}
	return printJSON(orderdomain.InvestigationEvidence{
		Order:      order,
		Payment:    payment,
		Inventory:  inventory,
		Fulfilment: fulfilment,
	})
}

func runAgent(ctx context.Context, tracing *telemetry.Runtime, advanced bool, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	shippingEndpoint := flags.String("shipping-endpoint", defaultShippingEndpoint, "shipping MCP endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	services := orderdata.NewDeterministicServices()
	var agentTools []tool.Tool
	var shipping *shippingmcp.Client
	if advanced {
		var err error
		shipping, err = shippingmcp.Connect(ctx, *shippingEndpoint)
		if err != nil {
			return err
		}
		defer shipping.Close()
		agentTools = append(ordertool.ReadOnlyTools(services), shipping.Tools()...)
	} else {
		agentTools = []tool.Tool{ordertool.NewGetOrder(services)}
	}
	client, err := orderagent.NewFromEnvironment(ctx, agentTools, tracing.AgentMiddlewares())
	if err != nil {
		return err
	}
	prompts := flags.Args()
	if len(prompts) == 0 {
		prompts = []string{"Why hasn't order 58372 shipped, and what can we do about it?"}
	}
	for _, prompt := range prompts {
		fmt.Printf("\n> %s\n\n", prompt)
		if _, err := client.Chat(ctx, prompt, func(update string) {
			fmt.Print(update)
		}); err != nil {
			return err
		}
		fmt.Println()
	}
	return nil
}

func runWorkflow(ctx context.Context, tracing *telemetry.Runtime, args []string) error {
	flags := flag.NewFlagSet("workflow", flag.ContinueOnError)
	shippingEndpoint := flags.String("shipping-endpoint", defaultShippingEndpoint, "shipping MCP endpoint")
	orderID := flags.String("order", orderdomain.DemoOrderID, "order identifier")
	if err := flags.Parse(args); err != nil {
		return err
	}
	services := orderdata.NewDeterministicServices()
	shipping, err := shippingmcp.Connect(ctx, *shippingEndpoint)
	if err != nil {
		return err
	}
	defer shipping.Close()
	reasoner, err := orderagent.NewFromEnvironment(ctx, nil, tracing.AgentMiddlewares())
	if err != nil {
		return err
	}
	wf, err := investigation.BuildInvestigationWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: shipping,
		Reasoner: reasoner,
		Tracer:   tracing.WorkflowTracer,
	})
	if err != nil {
		return err
	}
	result, err := executeInvestigation(ctx, wf, *orderID)
	if err != nil {
		return err
	}
	fmt.Println("\nInvestigation result:")
	return printJSON(result)
}

func runResolve(ctx context.Context, tracing *telemetry.Runtime, args []string) error {
	flags := flag.NewFlagSet("resolve", flag.ContinueOnError)
	shippingEndpoint := flags.String("shipping-endpoint", defaultShippingEndpoint, "shipping MCP endpoint")
	orderID := flags.String("order", orderdomain.DemoOrderID, "order identifier")
	stateDir := flags.String("state", filepath.Join(orderdomain.DefaultCheckpointFolder, orderdomain.DemoOrderID), "durable local state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	services := orderdata.NewDeterministicServices()
	shipping, err := shippingmcp.Connect(ctx, *shippingEndpoint)
	if err != nil {
		return err
	}
	defer shipping.Close()
	reasoner, err := orderagent.NewFromEnvironment(ctx, nil, tracing.AgentMiddlewares())
	if err != nil {
		return err
	}
	wf, err := investigation.BuildResolutionWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: shipping,
		Reasoner: reasoner,
		Tracer:   tracing.WorkflowTracer,
	})
	if err != nil {
		return err
	}
	paused, err := resolution.Start(ctx, wf, *stateDir, *orderID, printWorkflowProgress)
	if err != nil {
		return err
	}
	fmt.Println("\nApproval required:")
	if err := printJSON(paused.Approval); err != nil {
		return err
	}
	fmt.Printf("\nApprove:\n  go run ./cmd/order-demo resume --state %q --approve --actor operator@example.com --permission %s\n", *stateDir, orderdomain.TransferPermission)
	fmt.Printf("Reject:\n  go run ./cmd/order-demo resume --state %q --reject --actor operator@example.com\n", *stateDir)
	return nil
}

func runResume(ctx context.Context, tracing *telemetry.Runtime, args []string) error {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	stateDir := flags.String("state", filepath.Join(orderdomain.DefaultCheckpointFolder, orderdomain.DemoOrderID), "durable local state directory")
	approve := flags.Bool("approve", false, "approve the proposed transfer")
	reject := flags.Bool("reject", false, "reject the proposed transfer")
	actor := flags.String("actor", "", "approver identity")
	permission := flags.String("permission", "", "approver permission")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *approve == *reject {
		return fmt.Errorf("choose exactly one of --approve or --reject")
	}
	var permissions []string
	if *permission != "" {
		permissions = []string{*permission}
	}
	services := orderdata.NewDeterministicServices()
	wf, err := investigation.BuildResolutionWorkflow(investigation.Dependencies{
		Services: services,
		Shipping: unavailableShipping{},
		Reasoner: unavailableReasoner{},
		Tracer:   tracing.WorkflowTracer,
	})
	if err != nil {
		return err
	}
	result, err := resolution.Resume(
		ctx,
		wf,
		*stateDir,
		*approve,
		*actor,
		permissions,
		printWorkflowProgress,
	)
	if err != nil {
		return err
	}
	fmt.Println("\nResolution result:")
	return printJSON(result)
}

func executeInvestigation(ctx context.Context, wf *workflow.Workflow, orderID string) (investigation.InvestigationResult, error) {
	run, err := inproc.Default.RunStreaming(ctx, wf, orderID)
	if err != nil {
		return investigation.InvestigationResult{}, err
	}
	defer run.Close(ctx)
	var result investigation.InvestigationResult
	for event, eventErr := range run.WatchStream(ctx) {
		if eventErr != nil {
			return investigation.InvestigationResult{}, eventErr
		}
		printWorkflowProgress(event)
		switch value := event.(type) {
		case workflow.OutputEvent:
			typed, ok := value.Output.(investigation.InvestigationResult)
			if !ok {
				return investigation.InvestigationResult{}, fmt.Errorf("unexpected workflow output %T", value.Output)
			}
			result = typed
		case workflow.ErrorEvent:
			return investigation.InvestigationResult{}, value.Error
		case workflow.ExecutorFailedEvent:
			return investigation.InvestigationResult{}, value.Error
		}
	}
	if result.Evidence.Order.ID == "" {
		return investigation.InvestigationResult{}, fmt.Errorf("workflow completed without an investigation result")
	}
	return result, nil
}

func printWorkflowProgress(event workflow.Event) {
	switch value := event.(type) {
	case workflow.ExecutorInvokedEvent:
		if strings.HasPrefix(value.ExecutorID, "Check") {
			fmt.Printf("[workflow] goroutine branch started: %s\n", value.ExecutorID)
		}
	case workflow.ExecutorCompletedEvent:
		if strings.HasPrefix(value.ExecutorID, "Check") {
			fmt.Printf("[workflow] goroutine branch completed: %s\n", value.ExecutorID)
		}
	case investigation.AgentUpdateEvent:
		fmt.Print(value.Text)
	}
}

func printJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

type unavailableReasoner struct{}

func (unavailableReasoner) Recommend(
	context.Context,
	orderdomain.InvestigationEvidence,
	orderagent.StreamFunc,
) (orderdomain.Recommendation, error) {
	return orderdomain.Recommendation{}, errors.New("reasoner was unexpectedly invoked after checkpoint resume")
}

type unavailableShipping struct{}

func (unavailableShipping) GetShippingStatus(context.Context, string) (orderdomain.ShippingStatus, error) {
	return orderdomain.ShippingStatus{}, errors.New("shipping was unexpectedly invoked after checkpoint resume")
}

func (unavailableShipping) EstimateDelivery(context.Context, string, string) (orderdomain.DeliveryEstimate, error) {
	return orderdomain.DeliveryEstimate{}, errors.New("shipping was unexpectedly invoked after checkpoint resume")
}

func printUsage() {
	fmt.Print(`Order exception companion sample

Usage:
  order-demo domain
  order-demo basic [prompt ...]
  order-demo agent [--shipping-endpoint URL] [prompt ...]
  order-demo workflow [--shipping-endpoint URL] [--order ID]
  order-demo resolve [--shipping-endpoint URL] [--order ID] [--state DIR]
  order-demo resume --state DIR (--approve|--reject) --actor NAME [--permission NAME]
`)
}
