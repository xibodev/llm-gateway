package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

// streamPlayground streams a playground request through the data plane's own
// stream writer for its surface, so it is routed, metered and attributed as
// any stream is, and relays it to the console as playground events (see
// playgroundRelay). The request was authorized, admitted and checked for
// compatibility as one that does not stream is.
func streamPlayground(
	w http.ResponseWriter, r *http.Request, payload map[string]any, body playgroundBody,
	principal *config.Principal, project iam.Project, source string, surface core.ModelSurface,
	targets []router.Target, started time.Time,
) {
	ctx, sink := router.WithAttemptTrace(r.Context())
	relay := &playgroundRelay{
		w: w, header: http.Header{}, surface: surface, principal: principal, project: project,
		sink: sink, started: started, assembler: newPlaygroundAssembler(surface),
	}
	endpoint := "playground." + playgroundSurfaceName(surface)
	switch surface {
	case core.ModelSurfaceResponses:
		streamResponsesSSE(relay, ctx, targets, payload, payload, body.Model, principal, started, endpoint)
	case core.ModelSurfaceMessages:
		var request anthropicRequest
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &request)
		native, msgs, kw, lossErr := messagesStreamPlan(targets, principal, payload, &request)
		if lossErr != nil {
			writeError(w, http.StatusBadRequest, "Streaming cannot preserve Anthropic request: "+lossErr.Error())
			return
		}
		if native {
			streamNativeMessagesSSE(relay, ctx, targets, payload, body.Model, principal, started, endpoint)
		} else {
			streamMessagesSSE(relay, ctx, targets, msgs, body.Model, principal, kw, started, endpoint)
		}
	default:
		var request chatRequest
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &request)
		if len(request.Messages) == 0 {
			writeError(w, http.StatusBadRequest, "at least one message is required")
			return
		}
		streamChatSSE(relay, ctx, targets, providerMessages(request.Messages), body.Model, principal, chatKwargs(&request), endpoint, started)
	}
	detail := map[string]any{"model": body.Model, "surface": playgroundSurfacePath(surface), "source": source, "stream": true}
	switch relay.finish() {
	case "completed":
		detail["served_provider"], detail["served_model"] = relay.served.Provider, relay.served.Model
		auditPlayground(r, source, "playground.execute", project.ID, principal.PrincipalID, "success", detail)
	case "failed":
		auditPlayground(r, source, "playground.execute", project.ID, principal.PrincipalID, "failure", detail)
	}
}

// playgroundRelay is the ResponseWriter a data-plane stream writer writes a
// playground stream to. Until the stream opens, what the writer writes, a
// refusal, goes to the console as it is. Once it opens, the relay reads the
// surface's server-sent events as they are written and sends the console
// playground events instead: route as the stream opens, with the target that
// serves it and the route members tried; delta with each piece of text and
// reasoning; then done, with the answer assembled as a request that did not
// stream returns it, or error. A stream the console left ends with neither.
type playgroundRelay struct {
	w         http.ResponseWriter
	header    http.Header
	surface   core.ModelSurface
	principal *config.Principal
	project   iam.Project
	sink      *router.AttemptTraceSink
	started   time.Time
	assembler playgroundAssembler

	status    int
	streaming bool
	pending   []byte
	served    router.Target
	outcome   string
	failure   string
	// err is the first failed write to the console, which has gone away.
	err error
}

func (p *playgroundRelay) Header() http.Header { return p.header }

func (p *playgroundRelay) WriteHeader(status int) {
	if p.status != 0 {
		return
	}
	p.status = status
	if status != http.StatusOK || !strings.HasPrefix(p.header.Get("Content-Type"), "text/event-stream") {
		for name, values := range p.header {
			p.w.Header()[name] = values
		}
		p.w.WriteHeader(status)
		return
	}
	p.streaming = true
	p.w.Header().Set("Content-Type", "text/event-stream")
	p.w.Header().Set("Cache-Control", "no-cache")
	p.w.WriteHeader(http.StatusOK)
	trace := p.sink.Attempts()
	for _, attempt := range trace {
		if attempt.Status == "served" {
			p.served = router.Target{Provider: attempt.Provider, Model: attempt.Model}
		}
	}
	route := p.route()
	route["fallback_trace"] = trace
	p.send("route", route)
}

func (p *playgroundRelay) Write(data []byte) (int, error) {
	if p.status == 0 {
		p.WriteHeader(http.StatusOK)
	}
	if !p.streaming {
		return p.w.Write(data)
	}
	if p.err != nil {
		return 0, p.err
	}
	p.pending = append(p.pending, bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))...)
	for {
		end := bytes.Index(p.pending, []byte("\n\n"))
		if end < 0 {
			break
		}
		record := string(p.pending[:end])
		p.pending = p.pending[end+2:]
		p.consume(record)
		if p.err != nil {
			return 0, p.err
		}
	}
	return len(data), nil
}

// consume reads one server-sent event of the surface's stream.
func (p *playgroundRelay) consume(record string) {
	event, data := "", []string{}
	for _, line := range strings.Split(record, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 || p.outcome != "" {
		return
	}
	piece := p.assembler.consume(event, strings.Join(data, "\n"))
	if piece.text != "" || piece.reasoning != "" {
		delta := map[string]any{}
		if piece.text != "" {
			delta["text"] = piece.text
		}
		if piece.reasoning != "" {
			delta["reasoning"] = piece.reasoning
		}
		p.send("delta", delta)
	}
	if piece.failure != "" {
		p.outcome, p.failure = "failed", piece.failure
	} else if piece.completed {
		p.outcome = "completed"
	}
}

// finish ends a stream that opened with done or error, and reports how it
// ended: "completed", "failed", or "" for one that never opened, that the
// console left, or that a refusal ended before it opened, which reports
// "failed".
func (p *playgroundRelay) finish() string {
	if !p.streaming {
		if p.status >= http.StatusBadRequest {
			return "failed"
		}
		return ""
	}
	switch p.outcome {
	case "completed":
		answer := p.assembler.answer()
		done := p.route()
		done["project_id"], done["principal_id"] = p.project.ID, p.principal.PrincipalID
		done["latency_ms"] = time.Since(p.started).Milliseconds()
		done["usage"] = safePlaygroundValue(answer["usage"])
		done["fallback_trace"] = p.sink.Attempts()
		done["raw_response"] = safePlaygroundValue(answer)
		p.send("done", done)
	case "failed":
		p.send("error", map[string]any{"error": map[string]any{"message": p.failure}})
	default:
		if p.err != nil {
			return ""
		}
		p.outcome = "failed"
		p.send("error", map[string]any{"error": map[string]any{"message": "The stream ended before the answer was complete."}})
	}
	return p.outcome
}

// route is what the route, done and error events share: the target that
// served the stream and how it was reached.
func (p *playgroundRelay) route() map[string]any {
	path := playgroundSurfacePath(p.surface)
	return map[string]any{
		"served":         map[string]any{"provider": p.served.Provider, "model": p.served.Model},
		"transport_mode": targetTransportMode(p.served, callerOf(p.principal), path),
		"surface":        path,
	}
}

func (p *playgroundRelay) send(event string, payload any) {
	if p.err != nil {
		return
	}
	encoded, _ := json.Marshal(payload)
	p.err = writeAndFlush(p.w, []byte("event: "+event+"\ndata: "+string(encoded)+"\n\n"))
}

// playgroundPiece is what one event of a surface's stream added: text and
// reasoning to show, and whether the stream completed or failed with it.
type playgroundPiece struct {
	text, reasoning string
	completed       bool
	failure         string
}

// playgroundAssembler reads a surface's stream and assembles its answer as
// the surface returns one that does not stream.
type playgroundAssembler interface {
	consume(event, data string) playgroundPiece
	answer() map[string]any
}

func newPlaygroundAssembler(surface core.ModelSurface) playgroundAssembler {
	switch surface {
	case core.ModelSurfaceResponses:
		return &responsesAssembler{}
	case core.ModelSurfaceMessages:
		return &messagesAssembler{blocks: map[int]map[string]any{}, inputs: map[int]*strings.Builder{}}
	}
	return &chatAssembler{tools: map[int]map[string]any{}}
}

// chatAssembler assembles a Chat Completions stream: content, reasoning and
// tool calls, which arrive in pieces by index, the finish reason and usage.
type chatAssembler struct {
	id, model          string
	created            any
	content, reasoning strings.Builder
	tools              map[int]map[string]any
	finish             any
	usage              any
}

func (a *chatAssembler) consume(_ string, data string) playgroundPiece {
	if strings.TrimSpace(data) == "[DONE]" {
		return playgroundPiece{completed: true}
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return playgroundPiece{}
	}
	if failure, ok := chunk["error"].(map[string]any); ok {
		return playgroundPiece{failure: streamFailureMessage(failure)}
	}
	if id, _ := chunk["id"].(string); id != "" {
		a.id = id
	}
	if model, _ := chunk["model"].(string); model != "" {
		a.model = model
	}
	if created, ok := chunk["created"]; ok && created != nil {
		a.created = created
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		a.usage = usage
	}
	var piece playgroundPiece
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if index, _ := choice["index"].(float64); index != 0 {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if text, _ := delta["content"].(string); text != "" {
			a.content.WriteString(text)
			piece.text += text
		}
		for _, field := range []string{"reasoning_content", "reasoning"} {
			if text, _ := delta[field].(string); text != "" {
				a.reasoning.WriteString(text)
				piece.reasoning += text
			}
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range calls {
			a.toolCall(rawCall)
		}
		if finish, ok := choice["finish_reason"]; ok && finish != nil {
			a.finish = finish
		}
	}
	return piece
}

// toolCall merges one piece of a tool call into the call of its index: its
// ID, type and name come once, its arguments in pieces.
func (a *chatAssembler) toolCall(raw any) {
	piece, _ := raw.(map[string]any)
	index := 0
	if value, ok := piece["index"].(float64); ok {
		index = int(value)
	}
	call := a.tools[index]
	if call == nil {
		call = map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}}
		a.tools[index] = call
	}
	if id, _ := piece["id"].(string); id != "" {
		call["id"] = id
	}
	if kind, _ := piece["type"].(string); kind != "" {
		call["type"] = kind
	}
	function, _ := call["function"].(map[string]any)
	fragment, _ := piece["function"].(map[string]any)
	if name, _ := fragment["name"].(string); name != "" {
		function["name"] = name
	}
	if arguments, _ := fragment["arguments"].(string); arguments != "" {
		function["arguments"] = function["arguments"].(string) + arguments
	}
}

func (a *chatAssembler) answer() map[string]any {
	message := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		indexes := make([]int, 0, len(a.tools))
		for index := range a.tools {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		calls := make([]any, 0, len(indexes))
		for _, index := range indexes {
			calls = append(calls, a.tools[index])
		}
		message["tool_calls"] = calls
	}
	answer := map[string]any{
		"id": a.id, "object": "chat.completion", "model": a.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": a.finish}},
	}
	if a.created != nil {
		answer["created"] = a.created
	}
	if a.usage != nil {
		answer["usage"] = a.usage
	}
	return answer
}

// responsesAssembler reads a Responses stream, whose terminal event carries
// the whole response.
type responsesAssembler struct{ response map[string]any }

func (a *responsesAssembler) consume(event, data string) playgroundPiece {
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return playgroundPiece{}
	}
	kind, _ := payload["type"].(string)
	if kind == "" {
		kind = event
	}
	delta, _ := payload["delta"].(string)
	switch kind {
	case "response.output_text.delta":
		return playgroundPiece{text: delta}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return playgroundPiece{reasoning: delta}
	case "response.completed", "response.incomplete":
		a.response, _ = payload["response"].(map[string]any)
		return playgroundPiece{completed: true}
	case "response.failed":
		response, _ := payload["response"].(map[string]any)
		failure, _ := response["error"].(map[string]any)
		return playgroundPiece{failure: streamFailureMessage(failure)}
	}
	return playgroundPiece{}
}

func (a *responsesAssembler) answer() map[string]any {
	if a.response == nil {
		return map[string]any{}
	}
	return a.response
}

// messagesAssembler assembles a Messages stream: the message it starts, its
// content blocks, built by index from their deltas, and how it stopped.
type messagesAssembler struct {
	message map[string]any
	blocks  map[int]map[string]any
	inputs  map[int]*strings.Builder
}

func (a *messagesAssembler) consume(event, data string) playgroundPiece {
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return playgroundPiece{}
	}
	kind, _ := payload["type"].(string)
	if kind == "" {
		kind = event
	}
	index := 0
	if value, ok := payload["index"].(float64); ok {
		index = int(value)
	}
	switch kind {
	case "message_start":
		a.message, _ = payload["message"].(map[string]any)
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		copied := map[string]any{}
		for key, value := range block {
			copied[key] = value
		}
		a.blocks[index] = copied
	case "content_block_delta":
		return a.delta(index, payload)
	case "content_block_stop":
		if input := a.inputs[index]; input != nil && a.blocks[index] != nil {
			var parsed any
			if json.Unmarshal([]byte(input.String()), &parsed) == nil {
				a.blocks[index]["input"] = parsed
			}
		}
	case "message_delta":
		if a.message == nil {
			a.message = map[string]any{}
		}
		delta, _ := payload["delta"].(map[string]any)
		for key, value := range delta {
			a.message[key] = value
		}
		if usage, ok := payload["usage"].(map[string]any); ok {
			merged, _ := a.message["usage"].(map[string]any)
			if merged == nil {
				merged = map[string]any{}
			}
			for key, value := range usage {
				merged[key] = value
			}
			a.message["usage"] = merged
		}
	case "message_stop":
		return playgroundPiece{completed: true}
	case "error":
		failure, _ := payload["error"].(map[string]any)
		return playgroundPiece{failure: streamFailureMessage(failure)}
	}
	return playgroundPiece{}
}

// delta adds one content block delta to its block.
func (a *messagesAssembler) delta(index int, payload map[string]any) playgroundPiece {
	block := a.blocks[index]
	if block == nil {
		block = map[string]any{}
		a.blocks[index] = block
	}
	delta, _ := payload["delta"].(map[string]any)
	switch delta["type"] {
	case "text_delta":
		text, _ := delta["text"].(string)
		current, _ := block["text"].(string)
		block["text"] = current + text
		return playgroundPiece{text: text}
	case "thinking_delta":
		text, _ := delta["thinking"].(string)
		current, _ := block["thinking"].(string)
		block["thinking"] = current + text
		return playgroundPiece{reasoning: text}
	case "signature_delta":
		block["signature"] = delta["signature"]
	case "input_json_delta":
		if a.inputs[index] == nil {
			a.inputs[index] = &strings.Builder{}
		}
		partial, _ := delta["partial_json"].(string)
		a.inputs[index].WriteString(partial)
	}
	return playgroundPiece{}
}

func (a *messagesAssembler) answer() map[string]any {
	answer := map[string]any{"type": "message", "role": "assistant"}
	for key, value := range a.message {
		answer[key] = value
	}
	indexes := make([]int, 0, len(a.blocks))
	for index := range a.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]any, 0, len(indexes))
	for _, index := range indexes {
		content = append(content, a.blocks[index])
	}
	answer["content"] = content
	return answer
}

// streamFailureMessage is the message of a stream's error, or a general one
// when it carries none.
func streamFailureMessage(failure map[string]any) string {
	if message, _ := failure["message"].(string); strings.TrimSpace(message) != "" {
		return message
	}
	return "The provider's stream failed."
}
