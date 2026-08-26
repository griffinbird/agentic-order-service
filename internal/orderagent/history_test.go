package orderagent_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// This protects the local-history path used by the Foundry client:
// DisableStoreOutput plus streaming must replay encrypted reasoning and the
// matching function call/result, never plaintext reasoning.
func TestStreamedLocalHistoryReplaysToolCallAndEncryptedReasoning(t *testing.T) {
	t.Parallel()
	const encrypted = "encrypted-reasoning-for-order-58372"
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		captured = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, err = io.WriteString(w, `event: response.created
data: {"type":"response.created","response":{"id":"resp_2","object":"response","created_at":1756752901,"status":"in_progress","model":"o4-mini","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_2","status":"in_progress","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"Sydney is the supported option."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_2","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Sydney is the supported option.","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","object":"response","created_at":1756752901,"status":"completed","model":"o4-mini","output":[{"type":"message","id":"msg_2","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Sydney is the supported option.","annotations":[]}]}]}}

`)
		if err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	a := openaiprovider.NewResponsesAgent(
		openai.NewClient(option.WithBaseURL(server.URL)),
		openaiprovider.AgentConfig{
			Model:              "o4-mini",
			DisableStoreOutput: true,
			Config: agent.Config{
				DisableFuncAutoCall: true,
			},
		},
	)
	messages := []*message.Message{
		{
			Role: message.RoleUser,
			Contents: []message.Content{
				&message.TextContent{Text: "Why has order 58372 not shipped?"},
			},
		},
		{
			Role: message.RoleAssistant,
			Contents: []message.Content{
				&message.TextReasoningContent{
					Text:          "Plaintext reasoning must not be replayed.",
					ProtectedData: encrypted,
				},
				&message.FunctionCallContent{
					CallID:    "call_order_58372",
					Name:      "get_order",
					Arguments: `{"orderId":"58372"}`,
				},
			},
		},
		{
			Role: message.RoleTool,
			Contents: []message.Content{
				&message.FunctionResultContent{
					CallID: "call_order_58372",
					Result: `{"order":{"id":"58372","warehouse":"MEL"}}`,
				},
			},
		},
	}
	for _, err := range a.Run(t.Context(), messages, agent.Stream(true)) {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, required := range []string{
		`"store":false`,
		`"encrypted_content":"` + encrypted + `"`,
		`"summary":[]`,
		`"type":"function_call"`,
		`"call_id":"call_order_58372"`,
		`"type":"function_call_output"`,
	} {
		if !strings.Contains(captured, required) {
			t.Fatalf("request missing %s: %s", required, captured)
		}
	}
	if strings.Contains(captured, "Plaintext reasoning must not be replayed.") ||
		strings.Contains(captured, `"reasoning_text"`) {
		t.Fatalf("request replayed plaintext reasoning: %s", captured)
	}
}
