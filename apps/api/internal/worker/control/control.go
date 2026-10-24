// Package control is the Go client for the worker's service-control socket,
// wire-compatible with apps/control (protocol.ts and transport.ts): one JSON
// object per line, one connection per request, response read up to the first
// newline.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"
)

// Operation is a control operation; Operations mirrors CONTROL_OPERATIONS.
type Operation string

const (
	OperationStage    Operation = "stage"
	OperationValidate Operation = "validate"
	OperationPromote  Operation = "promote"
	OperationReload   Operation = "reload"
	OperationHealth   Operation = "health"
	OperationRollback Operation = "rollback"
)

// Operations lists every allowlisted operation in protocol order.
var Operations = []Operation{
	OperationStage, OperationValidate, OperationPromote,
	OperationReload, OperationHealth, OperationRollback,
}

// Service is a fixed controllable service.
type Service string

const (
	ServiceCoreDNS Service = "coredns"
	ServiceNginx   Service = "nginx"
)

// Request is the control request. Field order is the wire key order.
type Request struct {
	RequestID     string    `json:"requestId"`
	Operation     Operation `json:"operation"`
	Service       Service   `json:"service"`
	RevisionID    string    `json:"revisionId"`
	Checksum      string    `json:"checksum"`
	CandidatePath string    `json:"candidatePath"`
}

// Response is the control response. Output keeps the server's raw JSON bytes (nil
// when the key is absent, "null" for an explicit null) so numbers never shift.
type Response struct {
	RequestID string          `json:"requestId"`
	OK        bool            `json:"ok"`
	Operation Operation       `json:"operation"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// DefaultTimeout bounds one whole call (dial, write and read).
const DefaultTimeout = 10 * time.Second

// maxResponseBytes caps a response line. The TS client is unbounded; this keeps a
// misbehaving peer from exhausting memory.
const maxResponseBytes = 8 << 20

// MarshalRequest serializes one request line, newline included, byte-compatible
// with serializeControlRequest: compact JSON in declaration order and no HTML
// escaping. Go still writes U+2028/U+2029 as  /  (JSON.stringify emits
// them raw); both decode to the same string.
func MarshalRequest(request Request) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Client talks to the control socket.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithTimeout overrides the whole-call timeout.
func WithTimeout(timeout time.Duration) Option { return func(c *Client) { c.timeout = timeout } }

// NewClient builds a client for the unix socket at socketPath.
func NewClient(socketPath string, opts ...Option) *Client {
	c := &Client{socketPath: socketPath, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Execute sends one request on a fresh connection and returns the response. It
// rejects a response whose requestId or operation differs from the request.
func (c *Client) Execute(ctx context.Context, request Request) (Response, error) {
	line, err := MarshalRequest(request)
	if err != nil {
		return Response{}, err
	}
	call, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.exchange(call, line)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(call.Err(), context.DeadlineExceeded) {
			// The socket deadline can fire just before the context timer does.
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				return Response{}, context.DeadlineExceeded
			}
			return Response{}, errors.New("Control request timed out")
		}
		return Response{}, err
	}
	if response.RequestID != request.RequestID {
		return Response{}, errors.New("Control response request ID does not match")
	}
	if response.Operation != request.Operation {
		return Response{}, errors.New("Control response operation does not match")
	}
	return response, nil
}

func (c *Client) exchange(ctx context.Context, line []byte) (Response, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	// Closing the connection on cancellation unblocks a pending read or write.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write(line); err != nil {
		return Response{}, ctxOr(ctx, err)
	}
	reader := bufio.NewReader(io.LimitReader(conn, maxResponseBytes))
	text, err := reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && ctx.Err() == nil {
			return Response{}, errors.New("Control connection closed before a response line")
		}
		return Response{}, ctxOr(ctx, err)
	}
	var response Response
	if err := json.Unmarshal(text, &response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
