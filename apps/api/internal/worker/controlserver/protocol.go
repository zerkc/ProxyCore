// Package controlserver is the Go port of the apps/control server: it accepts
// newline-delimited JSON control requests on a Unix socket, dispatches them to
// fixed per-service handlers and replies with the exact bytes the TypeScript
// server (protocol.ts, transport.ts, service.ts) produces.
package controlserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

// maxRequestUnits is the TS limit on line.length, which counts UTF-16 code units.
const maxRequestUnits = 65_536

// maxRequestBytes is the most bytes a line within maxRequestUnits can occupy
// (3 UTF-8 bytes per UTF-16 unit). Longer lines are rejected without parsing.
const maxRequestBytes = maxRequestUnits * 3

// ParseError is a request the server rejects. Its text is the wire error string.
type ParseError struct {
	msg string
	// InvalidJSON marks a JSON syntax error (logged as invalid_json, a
	// SyntaxError in TS); every other rejection is invalid_request.
	InvalidJSON bool
}

func (e *ParseError) Error() string { return e.msg }

func rejected(msg string) error { return &ParseError{msg: msg} }

// utf16Len counts UTF-16 code units the way JavaScript's String.length does.
func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		b = b[size:]
	}
	return n
}

// ParseRequest mirrors parseControlRequest: it validates one line (without its
// newline) and returns the request, dropping unknown keys.
func ParseRequest(line []byte) (control.Request, error) {
	if len(line) > maxRequestBytes || utf16Len(line) > maxRequestUnits {
		return control.Request{}, rejected("Control request is too large")
	}
	if !json.Valid(line) {
		return control.Request{}, &ParseError{msg: jsonSyntaxMessage(line), InvalidJSON: true}
	}
	trimmed := bytes.TrimLeft(line, " \t\r\n")
	switch trimmed[0] {
	case '[':
		// An array has none of the keys: TS falls through to the field check.
		return control.Request{}, rejected("Control request fields are invalid")
	case '{':
	default:
		// `"command" in parsed` throws a TypeError for null and primitives.
		return control.Request{}, rejected(`parsed is not an Object. (evaluating '"command" in parsed')`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return control.Request{}, rejected("Invalid control request")
	}
	for _, key := range []string{"command", "shell", "args"} {
		if _, present := fields[key]; present {
			return control.Request{}, rejected("Arbitrary command fields are not allowed")
		}
	}
	var values [6]string
	for i, key := range []string{"requestId", "operation", "service", "revisionId", "checksum", "candidatePath"} {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &values[i]) != nil {
			return control.Request{}, rejected("Control request fields are invalid")
		}
	}
	request := control.Request{
		RequestID:     values[0],
		Operation:     control.Operation(values[1]),
		Service:       control.Service(values[2]),
		RevisionID:    values[3],
		Checksum:      values[4],
		CandidatePath: values[5],
	}
	known := false
	for _, op := range control.Operations {
		known = known || op == request.Operation
	}
	if !known {
		return control.Request{}, rejected("Unknown control operation: " + values[1])
	}
	if request.Service != control.ServiceCoreDNS && request.Service != control.ServiceNginx {
		return control.Request{}, rejected("Unknown control service: " + values[2])
	}
	path := request.CandidatePath
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "\x00") {
		return control.Request{}, rejected("Candidate path is invalid")
	}
	return request, nil
}

// jsonSyntaxMessage approximates Bun's (JavaScriptCore's) JSON.parse SyntaxError
// text for the common shapes. The engine's exact wording is not reproducible in
// general, so uncommon syntax errors share one generic message. The text only
// reaches the (ignored) error field of a requestId:"invalid" reply.
func jsonSyntaxMessage(line []byte) string {
	const prefix = "JSON Parse error: "
	var syntax *json.SyntaxError
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return prefix + "Unexpected EOF"
	}
	if c := trimmed[0]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' {
		end := 0
		for end < len(trimmed) && (trimmed[end] >= 'a' && trimmed[end] <= 'z' || trimmed[end] >= 'A' && trimmed[end] <= 'Z' ||
			trimmed[end] >= '0' && trimmed[end] <= '9' || trimmed[end] == '_' || trimmed[end] == '$') {
			end++
		}
		return prefix + `Unexpected identifier "` + string(trimmed[:end]) + `"`
	}
	err := json.Unmarshal(line, new(any))
	if errors.As(err, &syntax) && syntax.Offset >= int64(len(line)) {
		if trimmed[0] == '{' {
			return prefix + "Expected '}'"
		}
		return prefix + "Unexpected EOF"
	}
	return prefix + "Unable to parse JSON string"
}

// quoteJS encodes s as JSON.stringify does: no HTML escaping, U+2028/U+2029
// written raw, control characters as lowercase \u00xx.
func quoteJS(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Response is one reply. Output is the handler's raw JSON (nil omits the key,
// "null" keeps an explicit null) and is only written when OK; Error is always
// written when not OK, even if empty, as in TS.
type Response struct {
	RequestID string
	Operation control.Operation
	OK        bool
	Output    json.RawMessage
	Error     string
}

// MarshalResponse serializes one reply line, newline included, with the TS key
// order: requestId, operation, ok, then output or error.
func MarshalResponse(r Response) []byte {
	var b strings.Builder
	b.WriteString(`{"requestId":`)
	b.WriteString(quoteJS(r.RequestID))
	b.WriteString(`,"operation":`)
	b.WriteString(quoteJS(string(r.Operation)))
	if r.OK {
		b.WriteString(`,"ok":true`)
		if r.Output != nil {
			b.WriteString(`,"output":`)
			b.Write(r.Output)
		}
	} else {
		b.WriteString(`,"ok":false,"error":`)
		b.WriteString(quoteJS(r.Error))
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

// QuoteJS is the exported JSON.stringify-compatible string encoder, for backends
// that build raw JSON outputs.
func QuoteJS(s string) string { return quoteJS(s) }
