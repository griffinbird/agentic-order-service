package orderagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/foundryprovider"
	"github.com/microsoft/agent-framework-go/tool"
)

const instructions = `You investigate order fulfilment exceptions.
Use tools before making factual claims about an order.
Keep deterministic business rules in the tools and report explicit tool errors.
State what is known, what is missing, and the safest next check.
Never claim that a consequential action has run.`

type StreamFunc func(string)

type Reasoner interface {
	Recommend(context.Context, orderdomain.InvestigationEvidence, StreamFunc) (orderdomain.Recommendation, error)
}

type Client struct {
	agent   *agent.Agent
	session *agent.Session
}

func NewFromEnvironment(ctx context.Context, tools []tool.Tool, middlewares []agent.Middleware) (*Client, error) {
	endpoint := strings.TrimSpace(os.Getenv("FOUNDRY_PROJECT_ENDPOINT"))
	model := strings.TrimSpace(os.Getenv("FOUNDRY_MODEL"))
	if endpoint == "" {
		return nil, fmt.Errorf("FOUNDRY_PROJECT_ENDPOINT is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("FOUNDRY_PROJECT_ENDPOINT must be an absolute URL")
	}
	if model == "" {
		return nil, fmt.Errorf("FOUNDRY_MODEL is required")
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create DefaultAzureCredential: %w", err)
	}
	a := foundryprovider.NewAgent(
		endpoint,
		credential,
		foundryprovider.ModelDeployment(model),
		foundryprovider.AgentConfig{
			Instructions:       instructions,
			DisableStoreOutput: true,
			Config: agent.Config{
				Name:        "OrderExceptionAgent",
				Description: "Investigates order fulfilment exceptions using supplied capabilities.",
				Middlewares: middlewares,
				Tools:       tools,
			},
		},
	)
	session, err := a.CreateSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("create local agent session: %w", err)
	}
	return &Client{agent: a, session: session}, nil
}

func (c *Client) Chat(ctx context.Context, prompt string, stream StreamFunc) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("%w: prompt is required", orderdomain.ErrInvalidArgument)
	}
	var result strings.Builder
	for update, err := range c.agent.RunText(
		ctx,
		prompt,
		agent.WithSession(c.session),
		agent.Stream(true),
	) {
		if err != nil {
			return "", err
		}
		text := update.String()
		result.WriteString(text)
		if stream != nil && text != "" {
			stream(text)
		}
	}
	return result.String(), nil
}

func (c *Client) Recommend(ctx context.Context, evidence orderdomain.InvestigationEvidence, stream StreamFunc) (orderdomain.Recommendation, error) {
	evidenceJSON, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return orderdomain.Recommendation{}, fmt.Errorf("marshal investigation evidence: %w", err)
	}
	prompt := `Reason only over the supplied structured evidence.
Choose either "transfer_fulfilment" or "no_action".
When choosing transfer_fulfilment, populate transferProposal exactly from the evidence:
the current warehouse is fromWarehouse, the stocked alternate is toWarehouse,
and the order ID, first item SKU/quantity/version, delivery impact, cost, and currency must match.
Do not invent identifiers, facts, or completed actions.

Evidence:
` + string(evidenceJSON)

	var recommendation orderdomain.Recommendation
	for update, runErr := range c.agent.RunText(
		ctx,
		prompt,
		agent.WithSession(c.session),
		agent.WithStructuredOutput(&recommendation),
		agent.Stream(true),
	) {
		if runErr != nil {
			return orderdomain.Recommendation{}, runErr
		}
		if stream != nil {
			if text := update.String(); text != "" {
				stream(text)
			}
		}
	}
	return recommendation, nil
}
