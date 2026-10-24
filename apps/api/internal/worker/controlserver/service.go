package controlserver

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

// Handler runs one fixed operation. A nil output omits the response's output
// key (JS undefined); json.RawMessage("null") sends an explicit null.
type Handler func(ctx context.Context, request control.Request) (json.RawMessage, error)

// Handlers maps service and operation to the handler that implements it.
type Handlers map[control.Service]map[control.Operation]Handler

// Backend supplies the handler table. The Docker backend (T4b) implements it.
type Backend interface {
	Handlers() Handlers
}

// Dispatcher mirrors FixedServiceControl.
type Dispatcher struct {
	handlers Handlers
}

// NewDispatcher builds a dispatcher over handlers (nil is an empty table).
func NewDispatcher(handlers Handlers) *Dispatcher { return &Dispatcher{handlers: handlers} }

const operationFailed = "Control operation failed"

// Execute runs the handler for request and folds every failure into the reply.
func (d *Dispatcher) Execute(ctx context.Context, request control.Request) Response {
	response := Response{RequestID: request.RequestID, Operation: request.Operation}
	handler := d.handlers[request.Service][request.Operation]
	if handler == nil {
		response.Error = "Operation " + string(request.Operation) + " is unavailable for " + string(request.Service)
		return response
	}
	output, err := run(ctx, handler, request)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	if output != nil {
		var compact bytes.Buffer
		if json.Compact(&compact, output) != nil {
			response.Error = operationFailed
			return response
		}
		output = compact.Bytes()
	}
	response.OK, response.Output = true, output
	return response
}

// run calls the handler and turns a panic into the generic failure.
func run(ctx context.Context, handler Handler, request control.Request) (output json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			output, err = nil, errFailed
		}
	}()
	return handler(ctx, request)
}

type failure string

func (f failure) Error() string { return string(f) }

const errFailed = failure(operationFailed)

// FixedBoundaryHandlers is the fallback table of index.ts: every operation of
// both services is accepted and echoes the request.
func FixedBoundaryHandlers() Handlers {
	fixed := func(_ context.Context, request control.Request) (json.RawMessage, error) {
		return json.RawMessage(`{"service":` + quoteJS(string(request.Service)) +
			`,"operation":` + quoteJS(string(request.Operation)) +
			`,"candidatePath":` + quoteJS(request.CandidatePath) +
			`,"status":"accepted-by-fixed-boundary"}`), nil
	}
	handlers := Handlers{}
	for _, service := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		handlers[service] = map[control.Operation]Handler{}
		for _, operation := range control.Operations {
			handlers[service][operation] = fixed
		}
	}
	return handlers
}
