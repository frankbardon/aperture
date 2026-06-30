package gosdk

import (
	"context"
	"encoding/json"
	"errors"

	aerr "github.com/frankbardon/aperture/errors"
	core "github.com/frankbardon/aperture/mcp"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/service"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// errNilServer / errNilService are the two Register guard failures.
var (
	errNilServer  = errors.New("gosdk: Register requires a non-nil *mcp.Server")
	errNilService = errors.New("gosdk: Register requires a non-nil *service.Service")
)

// RegisteredTools returns the canonical list of MCP tool names this adapter
// mounts, in stable order. Mirrors toolmeta.Names().
func RegisteredTools() []string {
	return toolmeta.Names()
}

// registerTools mounts every tool onto the server by iterating the SDK-free core
// catalog. Each core descriptor carries a precomputed input schema (the reflected
// typed In) and a type-erased Invoke that decodes the raw arguments and calls the
// typed handler. This adapter owns only the go-sdk wiring: it adapts the raw
// CallToolRequest into core.Invoke and renders the typed result (or coded error)
// back into a CallToolResult. Handler logic lives in the core package.
//
// The generic AddTool[In,Out] reflection path is deliberately avoided (it would
// re-reflect — and could panic on — the contract types); the low-level
// Server.AddTool path accepts the json.RawMessage schema directly.
func registerTools(s *mcpsdk.Server, svc *service.Service, cfg Config) {
	for _, d := range core.Tools(cfg.Core()) {
		s.AddTool(
			&mcpsdk.Tool{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			},
			coreHandler(svc, d),
		)
	}
}

// coreHandler adapts one core.ToolDescriptor into a go-sdk ToolHandler. The raw
// structured tool arguments flow straight into the descriptor's Invoke. On
// success the typed Out is JSON-marshalled into a text result; on error a
// *errors.CodedError is rendered as the structured {code, message, context}
// envelope (verbatim — never string-flattened), and any other error becomes a
// plain tool-error result the LLM can self-correct against.
func coreHandler(svc *service.Service, d core.ToolDescriptor) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		result, err := d.Invoke(ctx, svc, rawArguments(req))
		if err != nil {
			return errorResultFor(err), nil
		}
		return jsonResult(result)
	}
}

// rawArguments returns the raw structured JSON arguments object the client sent.
// The tool contract is the reflected typed input (fields at the top level), so
// the blob is the typed In verbatim. A nil/empty blob yields an empty JSON object
// so decode falls through to the handler's own required-field guards.
func rawArguments(req *mcpsdk.CallToolRequest) json.RawMessage {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(req.Params.Arguments)
}

// codedEnvelope is the structured rendering of a *errors.CodedError: the stable
// {code, message, context} shape the caller receives instead of a bare string.
// Inner is intentionally omitted (it folds into message via the CodedError's
// Error()), so the envelope marshals cleanly.
type codedEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

// errorResultFor renders an Invoke error into a tool-error CallToolResult. A coded
// error is serialised as its structured envelope; any other error becomes bare
// text.
func errorResultFor(err error) *mcpsdk.CallToolResult {
	var ce *aerr.CodedError
	if errors.As(err, &ce) {
		env := codedEnvelope{Code: string(ce.Code), Message: ce.Error(), Context: ce.Context}
		if body, mErr := json.Marshal(env); mErr == nil {
			return errorResult(string(body))
		}
		return errorResult(ce.Error())
	}
	return errorResult(err.Error())
}

// textResult builds a successful single-text-content tool result.
func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}
}

// errorResult builds a tool-error result (IsError set) carrying text. Tool errors
// ride in Content per the MCP contract so the LLM can self-correct; they are not
// surfaced as protocol-level errors.
func errorResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		IsError: true,
	}
}

// jsonResult marshals v and returns it as text content.
func jsonResult(v any) (*mcpsdk.CallToolResult, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return errorResult("encode result: " + err.Error()), nil
	}
	return textResult(string(body)), nil
}
