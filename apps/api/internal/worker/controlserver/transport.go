package controlserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Server is a listening control socket.
type Server struct {
	listener   net.Listener
	dispatcher *Dispatcher
	log        func(string)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Listen prepares the socket like startControlServer: mkdir -p of the parent
// (default mode, so umask applies), unlink any previous socket ignoring errors,
// then listen. The socket file gets no explicit chmod, as in TS. log may be nil.
func Listen(socketPath string, dispatcher *Dispatcher, log func(string)) (*Server, error) {
	if log == nil {
		log = func(string) {}
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o777); err != nil {
		log("control socket failed reason=setup_failed stage=mkdir")
		return nil, err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log("control socket failed reason=setup_failed stage=listen")
		return nil, err
	}
	return &Server{listener: listener, dispatcher: dispatcher, log: log, conns: map[net.Conn]struct{}{}}, nil
}

// Close stops listening and drops every open connection. It is idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.listener.Close()
	for conn := range s.conns {
		conn.Close()
	}
	return err
}

// Serve accepts connections until ctx is cancelled (returning nil) or the
// listener fails. It returns only after every connection goroutine has ended.
func (s *Server) Serve(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !s.track(conn) {
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.untrack(conn)
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// serveConn answers each newline-terminated line in order. A client half-close
// ends the connection after the lines already received were answered (TS
// destroys the socket on "end"); a trailing partial line is dropped.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	reader := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, tooLong, err := readLine(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !s.shuttingDown(ctx, err) {
				s.log("control socket failed reason=socket_error")
			}
			return
		}
		var response Response
		if tooLong {
			response = s.invalid(rejected("Control request is too large"))
		} else {
			response = s.handleLine(ctx, line)
		}
		if _, err := conn.Write(MarshalResponse(response)); err != nil {
			if !s.shuttingDown(ctx, err) {
				s.log("control socket failed reason=socket_error")
			}
			return
		}
	}
}

func (s *Server) shuttingDown(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, net.ErrClosed)
}

func (s *Server) invalid(err error) Response {
	reason := "invalid_request"
	var parseErr *ParseError
	if errors.As(err, &parseErr) && parseErr.InvalidJSON {
		reason = "invalid_json"
	}
	s.log("control request failed reason=" + reason)
	return Response{RequestID: "invalid", Operation: "health", Error: err.Error()}
}

func (s *Server) handleLine(ctx context.Context, line []byte) Response {
	request, err := ParseRequest(line)
	if err != nil {
		return s.invalid(err)
	}
	response := s.dispatcher.Execute(ctx, request)
	if !response.OK {
		s.log("control request failed request=" + safeLogID(request.RequestID) +
			" operation=" + string(request.Operation) + " service=" + string(request.Service) + " reason=operation_failed")
	}
	return response
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// safeLogID passes only short identifier-shaped request IDs into the log. The
// regexp's $ would also match before a final newline in some engines; Go's
// matches only at end of text, like the JS original.
func safeLogID(id string) string {
	if safeID.MatchString(id) {
		return id
	}
	return "[redacted]"
}

// readLine returns the next line without its newline. A line longer than
// maxRequestBytes is consumed and discarded, returning tooLong so the caller
// can reject it without buffering it. io.EOF (also for a trailing partial
// line) means the peer is done.
func readLine(reader *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxRequestBytes+1 {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			if tooLong {
				return nil, true, nil
			}
			return line[:len(line)-1], false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, false, err
		}
	}
}
