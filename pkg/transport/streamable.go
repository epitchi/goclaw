package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	protocolVersionHeader = "Mcp-Protocol-Version"
	sessionIDHeader       = "Mcp-Session-Id"
	lastEventIDHeader     = "Last-Event-ID"

	// reconnectGrowFactor is the multiplicative factor by which the delay increases after each attempt.
	// A value of 1.0 results in a constant delay, while a value of 2.0 would double it each time.
	// It must be 1.0 or greater if MaxRetries is greater than 0.
	reconnectGrowFactor = 1.5
	// reconnectMaxDelay caps the backoff delay, preventing it from growing indefinitely.
	reconnectMaxDelay = 30 * time.Second
)

var (
	// reconnectInitialDelay is the base delay for the first reconnect attempt.
	//
	// Mutable for testing.
	reconnectInitialDelay = 1 * time.Second
	// ErrParse is used when invalid JSON was received by the server.
	ErrParse = NewError(-32700, "parse error")
	// ErrInvalidRequest is used when the JSON sent is not a valid Request object.
	ErrInvalidRequest = NewError(-32600, "invalid request")
	// ErrMethodNotFound should be returned by the handler when the method does
	// not exist / is not available.
	ErrMethodNotFound = NewError(-32601, "method not found")
	// ErrInvalidParams should be returned by the handler when method
	// parameter(s) were invalid.
	ErrInvalidParams = NewError(-32602, "invalid params")
	// ErrInternal indicates a failure to process a call correctly
	ErrInternal = NewError(-32603, "internal error")

	// The following errors are not part of the json specification, but
	// compliant extensions specific to this implementation.

	// ErrServerOverloaded is returned when a message was refused due to a
	// server being temporarily unable to accept any new messages.
	ErrServerOverloaded = NewError(-32000, "overloaded")
	// ErrUnknown should be used for all non coded errors.
	ErrUnknown = NewError(-32001, "unknown error")
	// ErrServerClosing is returned for calls that arrive while the server is closing.
	ErrServerClosing = NewError(-32004, "server is closing")
	// ErrClientClosing is a dummy error returned for calls initiated while the client is closing.
	ErrClientClosing = NewError(-32003, "client is closing")

	// The following errors have special semantics for MCP transports

	// ErrRejected may be wrapped to return errors from calls to Writer.Write
	// that signal that the request was rejected by the transport layer as
	// invalid.
	//
	// Such failures do not indicate that the connection is broken, but rather
	// should be returned to the caller to indicate that the specific request is
	// invalid in the current context.
	ErrRejected = NewError(-32005, "rejected by transport")
)

// NewError returns an error that will encode on the wire correctly.
// The standard codes are made available from this package, this function should
// only be used to build errors for application specific codes as allowed by the
// specification.
func NewError(code int64, message string) error {
	return &WireError{
		Code:    code,
		Message: message,
	}
}

// WireError represents a structured error in a Response.
type WireError struct {
	// Code is an error code indicating the type of failure.
	Code int64 `json:"code"`
	// Message is a short description of the error.
	Message string `json:"message"`
	// Data is optional structured data containing additional information about the error.
	Data json.RawMessage `json:"data,omitempty"`
}

func (err *WireError) Error() string {
	return err.Message
}

func (err *WireError) Is(other error) bool {
	w, ok := other.(*WireError)
	if !ok {
		return false
	}
	return err.Code == w.Code
}

type streamableClientConn struct {
	url        string
	client     *http.Client
	ctx        context.Context    // connection context, detached from Connect
	cancel     context.CancelFunc // cancels ctx
	incoming   chan jsonrpc.Message
	maxRetries int
	strict     bool         // from [StreamableClientTransport.strict]
	logger     *slog.Logger // from [StreamableClientTransport.logger]

	// disableStandaloneSSE controls whether to disable the standalone SSE stream
	// for receiving server-to-client notifications when no request is in flight.
	disableStandaloneSSE bool // from [StreamableClientTransport.DisableStandaloneSSE]

	// Guard calls to Close, as it may be called multiple times.
	closeOnce sync.Once
	closeErr  error
	done      chan struct{} // signal graceful termination

	// Logical reads are distributed across multiple http requests. Whenever any
	// of them fails to process their response, we must break the connection, by
	// failing the pending Read.
	//
	// Achieve this by storing the failure message, and signalling when reads are
	// broken. See also [streamableClientConn.fail] and
	// [streamableClientConn.failure].
	failOnce sync.Once
	_failure error
	failed   chan struct{} // signal failure

	// Guard the initialization state.
	mu                sync.Mutex
	initializedResult *mcp.InitializeResult
	sessionID         string

	headers http.Header
}

type clientSessionState struct {
	InitializeResult *mcp.InitializeResult
}

// A ClientConnection is a [Connection] that is specific to the MCP client.
//
// If client connections implement this interface, they may receive information
// about changes to the client session.
//
// TODO: should this interface be exported?
type clientConnection interface {
	mcp.Connection

	// sessionUpdated is called whenever the client session state changes.
	sessionUpdated(clientSessionState)
}

// errSessionMissing distinguishes if the session is known to not be present on
// the server (see [streamableClientConn.fail]).
//
// TODO(rfindley): should we expose this error value (and its corresponding
// API) to the user?
//
// The spec says that if the server returns 404, clients should reestablish
// a session. For now, we delegate that to the user, but do they need a way to
// differentiate a 'NotFound' error from other errors?
var errSessionMissing = errors.New("session not found")

var _ clientConnection = (*streamableClientConn)(nil)

func (c *streamableClientConn) sessionUpdated(state clientSessionState) {
	c.mu.Lock()
	c.initializedResult = state.InitializeResult
	c.mu.Unlock()

	// Start the standalone SSE stream as soon as we have the initialized
	// result, if continuous listening is enabled.
	//
	// § 2.2: The client MAY issue an HTTP GET to the MCP endpoint. This can be
	// used to open an SSE stream, allowing the server to communicate to the
	// client, without the client first sending data via HTTP POST.
	//
	// We have to wait for initialized, because until we've received
	// initialized, we don't know whether the server requires a sessionID.
	//
	// § 2.5: A server using the Streamable HTTP transport MAY assign a session
	// ID at initialization time, by including it in a Mcp-Session-Id header
	// on the HTTP response containing the InitializeResult.
	if !c.disableStandaloneSSE {
		c.connectStandaloneSSE()
	}
}

func (c *streamableClientConn) connectStandaloneSSE() {
	resp, err := c.connectSSE(c.ctx, "", 0, true)
	if err != nil {
		// If the client didn't cancel the request, and failure breaks the logical
		// session.
		if c.ctx.Err() == nil {
			c.fail(fmt.Errorf("standalone SSE request failed (session ID: %v): %v", c.sessionID, err))
		}
		return
	}

	// [§2.2.3]: "The server MUST either return Content-Type:
	// text/event-stream in response to this HTTP GET, or else return HTTP
	// 405 Method Not Allowed, indicating that the server does not offer an
	// SSE stream at this endpoint."
	//
	// [§2.2.3]: https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#listening-for-messages-from-the-server
	if resp.StatusCode == http.StatusMethodNotAllowed {
		// The server doesn't support the standalone SSE stream.
		resp.Body.Close()
		return
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		// modelcontextprotocol/go-sdk#736: some servers return 200 OK or redirect with
		// non-SSE content type instead of text/event-stream for the standalone
		// SSE stream.
		c.logger.Warn(fmt.Sprintf("got Content-Type %s instead of text/event-stream for standalone SSE stream", resp.Header.Get("Content-Type")))
		resp.Body.Close()
		return
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && !c.strict {
		// modelcontextprotocol/go-sdk#393,#610: some servers return NotFound or
		// other status codes instead of MethodNotAllowed for the standalone SSE
		// stream.
		//
		// Treat this like MethodNotAllowed in non-strict mode.
		c.logger.Warn(fmt.Sprintf("got %d instead of 405 for standalone SSE stream", resp.StatusCode))
		resp.Body.Close()
		return
	}
	summary := "standalone SSE stream"
	if err := c.checkResponse(summary, resp); err != nil {
		c.fail(err)
		return
	}
	go c.handleSSE(c.ctx, summary, resp, nil)
}

// fail handles an asynchronous error while reading.
//
// If err is non-nil, it is terminal, and subsequent (or pending) Reads will
// fail.
//
// If err wraps errSessionMissing, the failure indicates that the session is no
// longer present on the server, and no final DELETE will be performed when
// closing the connection.
func (c *streamableClientConn) fail(err error) {
	if err != nil {
		c.failOnce.Do(func() {
			c._failure = err
			close(c.failed)
		})
	}
}

func (c *streamableClientConn) failure() error {
	select {
	case <-c.failed:
		return c._failure
	default:
		return nil
	}
}

func (c *streamableClientConn) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Read implements the [Connection] interface.
func (c *streamableClientConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.failed:
		return nil, c.failure()
	case <-c.done:
		return nil, io.EOF
	case msg := <-c.incoming:
		return msg, nil
	}
}

// Write implements the [Connection] interface.
func (c *streamableClientConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := c.failure(); err != nil {
		return err
	}

	var requestSummary string
	var forCall *jsonrpc.Request
	switch msg := msg.(type) {
	case *jsonrpc.Request:
		requestSummary = fmt.Sprintf("sending %q", msg.Method)
		if msg.IsCall() {
			forCall = msg
		}
	case *jsonrpc.Response:
		requestSummary = fmt.Sprintf("sending jsonrpc response #%d", msg.ID)
	default:
		panic("unreachable")
	}

	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("%s: %v", requestSummary, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.setMCPHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		// Any error from client.Do means the request didn't reach the server.
		// Wrap with ErrRejected so the jsonrpc2 connection doesn't set writeErr
		// and permanently break the connection.
		return fmt.Errorf("%w: %s: %v", ErrRejected, requestSummary, err)
	}

	if err := c.checkResponse(requestSummary, resp); err != nil {
		// Only fail the connection for non-transient errors.
		// Transient errors (wrapped with ErrRejected) should not break the connection.
		if !errors.Is(err, ErrRejected) {
			c.fail(err)
		}
		return err
	}

	if sessionID := resp.Header.Get(sessionIDHeader); sessionID != "" {
		c.mu.Lock()
		hadSessionID := c.sessionID
		if hadSessionID == "" {
			c.sessionID = sessionID
		}
		c.mu.Unlock()
		if hadSessionID != "" && hadSessionID != sessionID {
			resp.Body.Close()
			return fmt.Errorf("mismatching session IDs %q and %q", hadSessionID, sessionID)
		}
	}

	if forCall == nil {
		resp.Body.Close()

		// [§2.1.4]: "If the input is a JSON-RPC response or notification:
		// If the server accepts the input, the server MUST return HTTP status code 202 Accepted with no body."
		//
		// [§2.1.4]: https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#listening-for-messages-from-the-server
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
			errMsg := fmt.Sprintf("unexpected status code %d from non-call", resp.StatusCode)
			// Some servers return 200, even with an empty json body.
			//
			// In strict mode, return an error to the caller.
			c.logger.Warn(errMsg)
			if c.strict {
				return errors.New(errMsg)
			}
		}
		return nil
	}

	contentType := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	switch contentType {
	case "application/json":
		go c.handleJSON(requestSummary, resp)

	case "text/event-stream":
		var forCall *jsonrpc.Request
		if jsonReq, ok := msg.(*jsonrpc.Request); ok && jsonReq.IsCall() {
			forCall = jsonReq
		}
		// Handle the resulting stream. Note that ctx comes from the call, and
		// therefore is already cancelled when the JSON-RPC request is cancelled
		// (or rather, context cancellation is what *triggers* JSON-RPC
		// cancellation)
		go c.handleSSE(ctx, requestSummary, resp, forCall)

	default:
		resp.Body.Close()
		return fmt.Errorf("%s: unsupported content type %q", requestSummary, contentType)
	}
	return nil
}

func (c *streamableClientConn) setMCPHeaders(req *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initializedResult != nil {
		req.Header.Set(protocolVersionHeader, c.initializedResult.ProtocolVersion)
	}
	if c.sessionID != "" {
		req.Header.Set(sessionIDHeader, c.sessionID)
	}

	for key, value := range c.headers {
		for _, val := range value {
			req.Header.Add(key, val)
		}
	}
}

func (c *streamableClientConn) handleJSON(requestSummary string, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		c.fail(fmt.Errorf("%s: failed to read body: %v", requestSummary, err))
		return
	}
	msg, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		c.fail(fmt.Errorf("%s: failed to decode response: %v", requestSummary, err))
		return
	}
	select {
	case c.incoming <- msg:
	case <-c.done:
		// The connection was closed by the client; exit gracefully.
	}
}

// handleSSE manages the lifecycle of an SSE connection. It can be either
// persistent (for the main GET listener) or temporary (for a POST response).
//
// If forCall is set, it is the call that initiated the stream, and the
// stream is complete when we receive its response. Otherwise, this is the
// standalone stream.
func (c *streamableClientConn) handleSSE(ctx context.Context, requestSummary string, resp *http.Response, forCall *jsonrpc.Request) {
	for {
		// Connection was successful. Continue the loop with the new response.
		//
		// TODO(#679): we should set a reasonable limit on the number of times
		// we'll try getting a response for a given request, or enforce that we
		// actually make progress.
		//
		// Eventually, if we don't get the response, we should stop trying and
		// fail the request.
		lastEventID, reconnectDelay, clientClosed := c.processStream(ctx, requestSummary, resp, forCall)

		// If the connection was closed by the client, we're done.
		if clientClosed {
			return
		}
		// If we don't have a last event ID, we can never get the call response, so
		// there's nothing to resume. For the standalone stream, we can reconnect,
		// but we may just miss messages.
		if lastEventID == "" && forCall != nil {
			return
		}

		// The stream was interrupted or ended by the server. Attempt to reconnect.
		newResp, err := c.connectSSE(ctx, lastEventID, reconnectDelay, false)
		if err != nil {
			// If the client didn't cancel this request, any failure to execute it
			// breaks the logical MCP session.
			if ctx.Err() == nil {
				// All reconnection attempts failed: fail the connection.
				c.fail(fmt.Errorf("%s: failed to reconnect (session ID: %v): %v", requestSummary, c.sessionID, err))
			}
			return
		}

		resp = newResp
		if err := c.checkResponse(requestSummary, resp); err != nil {
			c.fail(err)
			return
		}
	}
}

// checkResponse checks the status code of the provided response, and
// translates it into an error if the request was unsuccessful.
//
// The response body is close if a non-nil error is returned.
func (c *streamableClientConn) checkResponse(requestSummary string, resp *http.Response) (err error) {
	defer func() {
		if err != nil {
			resp.Body.Close()
		}
	}()
	// §2.5.3: "The server MAY terminate the session at any time, after
	// which it MUST respond to requests containing that session ID with HTTP
	// 404 Not Found."
	if resp.StatusCode == http.StatusNotFound {
		// Return an errSessionMissing to avoid sending a redundant DELETE when the
		// session is already gone.
		return fmt.Errorf("%s: failed to connect (session ID: %v): %w", requestSummary, c.sessionID, errSessionMissing)
	}
	// Transient server errors (502, 503, 504, 429) should not break the connection.
	// Wrap them with ErrRejected so the jsonrpc2 layer doesn't set writeErr.
	if isTransientHTTPStatus(resp.StatusCode) {
		return fmt.Errorf("%w: %s: %v", ErrRejected, requestSummary, http.StatusText(resp.StatusCode))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %v", requestSummary, http.StatusText(resp.StatusCode))
	}
	return nil
}

// processStream reads from a single response body, sending events to the
// incoming channel. It returns the ID of the last processed event and a flag
// indicating if the connection was closed by the client. If resp is nil, it
// returns "", false.
func (c *streamableClientConn) processStream(ctx context.Context, requestSummary string, resp *http.Response, forCall *jsonrpc.Request) (lastEventID string, reconnectDelay time.Duration, clientClosed bool) {
	defer func() {
		// Drain any remaining unprocessed body. This allows the connection to be re-used after closing.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	for evt, err := range scanEvents(resp.Body) {
		if err != nil {
			if ctx.Err() != nil {
				return "", 0, true // don't reconnect: client cancelled
			}
			break
		}

		if evt.ID != "" {
			lastEventID = evt.ID
		}

		if evt.Retry != "" {
			if n, err := strconv.ParseInt(evt.Retry, 10, 64); err == nil {
				reconnectDelay = time.Duration(n) * time.Millisecond
			}
		}
		// According to SSE spec, events with no name default to "message"
		if evt.Name != "" && evt.Name != "message" {
			continue
		}

		msg, err := jsonrpc.DecodeMessage(evt.Data)
		if err != nil {
			c.fail(fmt.Errorf("%s: failed to decode event: %v", requestSummary, err))
			return "", 0, true
		}

		select {
		case c.incoming <- msg:
			// Check if this is the response to our call, which terminates the request.
			// (it could also be a server->client request or notification).
			if jsonResp, ok := msg.(*jsonrpc.Response); ok && forCall != nil {
				// TODO: we should never get a response when forReq is nil (the standalone SSE request).
				// We should detect this case.
				if jsonResp.ID == forCall.ID {
					return "", 0, true
				}
			}

		case <-c.done:
			// The connection was closed by the client; exit gracefully.
			return "", 0, true
		}
	}
	// The loop finished without an error, indicating the server closed the stream.
	//
	// If the lastEventID is "", the stream is not retryable and we should
	// report a synthetic error for the call.
	//
	// Note that this is different from the cancellation case above, since the
	// caller is still waiting for a response that will never come.
	if lastEventID == "" && forCall != nil {
		errmsg := &jsonrpc.Response{
			ID:    forCall.ID,
			Error: fmt.Errorf("request terminated without response"),
		}
		select {
		case c.incoming <- errmsg:
		case <-c.done:
		}
	}
	return lastEventID, reconnectDelay, false
}

// connectSSE handles the logic of connecting a text/event-stream connection.
//
// If lastEventID is set, it is the last-event ID of a stream being resumed.
//
// If connection fails, connectSSE retries with an exponential backoff
// strategy. It returns a new, valid HTTP response if successful, or an error
// if all retries are exhausted.
//
// reconnectDelay is the delay set by the server using the SSE retry field, or
// 0.
//
// If initial is set, this is the initial attempt.
//
// If connectSSE exits due to context cancellation, the result is (nil, ctx.Err()).
func (c *streamableClientConn) connectSSE(ctx context.Context, lastEventID string, reconnectDelay time.Duration, initial bool) (*http.Response, error) {
	var finalErr error
	attempt := 0
	if !initial {
		// We've already connected successfully once, so delay subsequent
		// reconnections. Otherwise, if the server returns 200 but terminates the
		// connection, we'll reconnect as fast as we can, ad infinitum.
		//
		// TODO: we should consider also setting a limit on total attempts for one
		// logical request.
		attempt = 1
	}
	delay := calculateReconnectDelay(attempt)
	if reconnectDelay > 0 {
		delay = reconnectDelay // honor the server's requested initial delay
	}
	for ; attempt <= c.maxRetries; attempt++ {
		select {
		case <-c.done:
			return nil, fmt.Errorf("connection closed by client during reconnect")

		case <-ctx.Done():
			// If the connection context is canceled, the request below will not
			// succeed anyway.
			return nil, ctx.Err()

		case <-time.After(delay):
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
			if err != nil {
				return nil, err
			}
			c.setMCPHeaders(req)
			if lastEventID != "" {
				req.Header.Set(lastEventIDHeader, lastEventID)
			}
			req.Header.Set("Accept", "text/event-stream")
			resp, err := c.client.Do(req)
			if err != nil {
				finalErr = err // Store the error and try again.
				delay = calculateReconnectDelay(attempt + 1)
				continue
			}
			return resp, nil
		}
	}
	// If the loop completes, all retries have failed, or the client is closing.
	if finalErr != nil {
		return nil, fmt.Errorf("connection failed after %d attempts: %w", c.maxRetries, finalErr)
	}
	return nil, fmt.Errorf("connection aborted after %d attempts", c.maxRetries)
}

// Close implements the [Connection] interface.
func (c *streamableClientConn) Close() error {
	c.closeOnce.Do(func() {
		if errors.Is(c.failure(), errSessionMissing) {
			// If the session is missing, no need to delete it.
		} else {
			req, err := http.NewRequestWithContext(c.ctx, http.MethodDelete, c.url, nil)
			if err != nil {
				c.closeErr = err
			} else {
				c.setMCPHeaders(req)
				if _, err := c.client.Do(req); err != nil {
					c.closeErr = err
				}
			}
		}

		// Cancel any hanging network requests after cleanup.
		c.cancel()
		close(c.done)
	})
	return c.closeErr
}

// calculateReconnectDelay calculates a delay using exponential backoff with full jitter.
func calculateReconnectDelay(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}
	// Calculate the exponential backoff using the grow factor.
	backoffDuration := time.Duration(float64(reconnectInitialDelay) * math.Pow(reconnectGrowFactor, float64(attempt-1)))
	// Cap the backoffDuration at maxDelay.
	backoffDuration = min(backoffDuration, reconnectMaxDelay)

	// Use a full jitter using backoffDuration
	jitter := rand.N(backoffDuration)

	return backoffDuration + jitter
}

// isTransientHTTPStatus reports whether the HTTP status code indicates a
// transient server error that should not permanently break the connection.
func isTransientHTTPStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
		http.StatusTooManyRequests:    // 429
		return true
	}
	return false
}

// scanEvents iterates SSE events in the given scanner. The iterated error is
// terminal: if encountered, the stream is corrupt or broken and should no
// longer be used.
//
// TODO(rfindley): consider a different API here that makes failure modes more
// apparent.
func scanEvents(r io.Reader) iter.Seq2[Event, error] {
	scanner := bufio.NewScanner(r)
	const maxTokenSize = 1 * 1024 * 1024 // 1 MiB max line size
	scanner.Buffer(nil, maxTokenSize)

	// TODO: investigate proper behavior when events are out of order, or have
	// non-standard names.
	var (
		eventKey = []byte("event")
		idKey    = []byte("id")
		dataKey  = []byte("data")
		retryKey = []byte("retry")
	)

	return func(yield func(Event, error) bool) {
		// iterate event from the wire.
		// https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events#examples
		//
		//  - `key: value` line records.
		//  - Consecutive `data: ...` fields are joined with newlines.
		//  - Unrecognized fields are ignored. Since we only care about 'event', 'id', and
		//   'data', these are the only three we consider.
		//  - Lines starting with ":" are ignored.
		//  - Records are terminated with two consecutive newlines.
		var (
			evt     Event
			dataBuf *bytes.Buffer // if non-nil, preceding field was also data
		)
		flushData := func() {
			if dataBuf != nil {
				evt.Data = dataBuf.Bytes()
				dataBuf = nil
			}
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				flushData()
				// \n\n is the record delimiter
				if !evt.Empty() && !yield(evt, nil) {
					return
				}
				evt = Event{}
				continue
			}
			before, after, found := bytes.Cut(line, []byte{':'})
			if !found {
				yield(Event{}, fmt.Errorf("malformed line in SSE stream: %q", string(line)))
				return
			}
			if !bytes.Equal(before, dataKey) {
				flushData()
			}
			switch {
			case bytes.Equal(before, eventKey):
				evt.Name = strings.TrimSpace(string(after))
			case bytes.Equal(before, idKey):
				evt.ID = strings.TrimSpace(string(after))
			case bytes.Equal(before, retryKey):
				evt.Retry = strings.TrimSpace(string(after))
			case bytes.Equal(before, dataKey):
				data := bytes.TrimSpace(after)
				if dataBuf != nil {
					dataBuf.WriteByte('\n')
					dataBuf.Write(data)
				} else {
					dataBuf = new(bytes.Buffer)
					dataBuf.Write(data)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				err = fmt.Errorf("event exceeded max line length of %d", maxTokenSize)
			}
			if !yield(Event{}, err) {
				return
			}
		}
		flushData()
		if !evt.Empty() {
			yield(evt, nil)
		}
	}
}

// An Event is a server-sent event.
// See https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events#fields.
type Event struct {
	Name  string // the "event" field
	ID    string // the "id" field
	Data  []byte // the "data" field
	Retry string // the "retry" field
}

// Empty reports whether the Event is empty.
func (e Event) Empty() bool {
	return e.Name == "" && e.ID == "" && len(e.Data) == 0 && e.Retry == ""
}

// A StreamableClientTransport is a [Transport] that can communicate with an MCP
// endpoint serving the streamable HTTP transport defined by the 2025-03-26
// version of the spec.
type StreamableClientTransport struct {
	Endpoint   string
	HTTPClient *http.Client
	Headers    http.Header
	// MaxRetries is the maximum number of times to attempt a reconnect before giving up.
	// It defaults to 5. To disable retries, use a negative number.
	MaxRetries int

	// DisableStandaloneSSE controls whether the client establishes a standalone SSE stream
	// for receiving server-initiated messages.
	//
	// When false (the default), after initialization the client sends an HTTP GET request
	// to establish a persistent server-sent events (SSE) connection. This allows the server
	// to send messages to the client at any time, such as ToolListChangedNotification or
	// other server-initiated requests and notifications. The connection persists for the
	// lifetime of the session and automatically reconnects if interrupted.
	//
	// When true, the client does not establish the standalone SSE stream. The client will
	// only receive responses to its own POST requests. Server-initiated messages will not
	// be received.
	//
	// According to the MCP specification, the standalone SSE stream is optional.
	// Setting DisableStandaloneSSE to true is useful when:
	//   - You only need request-response communication and don't need server-initiated notifications
	//   - The server doesn't properly handle GET requests for SSE streams
	//   - You want to avoid maintaining a persistent connection
	DisableStandaloneSSE bool

	// TODO(rfindley): propose exporting these.
	// If strict is set, the transport is in 'strict mode', where any violation
	// of the MCP spec causes a failure.
	strict bool
	// If logger is set, it is used to log aspects of the transport, such as spec
	// violations that were ignored.
	logger *slog.Logger
}

// Connect implements the [Transport] interface.
//
// The resulting [Connection] writes messages via POST requests to the
// transport URL with the Mcp-Session-Id header set, and reads messages from
// hanging requests.
//
// When closed, the connection issues a DELETE request to terminate the logical
// session.
func (t *StreamableClientTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	client := t.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	maxRetries := t.MaxRetries
	if maxRetries == 0 {
		maxRetries = 5
	} else if maxRetries < 0 {
		maxRetries = 0
	}
	// Create a new cancellable context that will manage the connection's lifecycle.
	// This is crucial for cleanly shutting down the background SSE listener by
	// cancelling its blocking network operations, which prevents hangs on exit.
	//
	// This context should be detached from the incoming context: the standalone
	// SSE request should not break when the connection context is done.
	//
	// For example, consider that the user may want to wait at most 5s to connect
	// to the server, and therefore uses a context with a 5s timeout when calling
	// client.Connect. Let's suppose that Connect returns after 1s, and the user
	// starts using the resulting session. If we didn't detach here, the session
	// would break after 4s, when the background SSE stream is terminated.
	//
	// Instead, creating a cancellable context detached from the incoming context
	// allows us to preserve context values (which may be necessary for auth
	// middleware), yet only cancel the standalone stream when the connection is closed.
	connCtx, cancel := context.WithCancel(ctx)
	conn := &streamableClientConn{
		url:                  t.Endpoint,
		client:               client,
		headers:              t.Headers,
		incoming:             make(chan jsonrpc.Message, 10),
		done:                 make(chan struct{}),
		maxRetries:           maxRetries,
		strict:               t.strict,
		logger:               ensureLogger(t.logger), // must be non-nil for safe logging
		ctx:                  connCtx,
		cancel:               cancel,
		failed:               make(chan struct{}),
		disableStandaloneSSE: t.DisableStandaloneSSE,
	}
	return conn, nil
}

// discardHandler is a slog.Handler that drops all logs.
// TODO: use slog.DiscardHandler when we require Go 1.24+.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

// ensureLogger returns l if non-nil, otherwise a discard logger.
func ensureLogger(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(discardHandler{})
}
