// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package comms

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/gorilla/websocket"
)

const (
	// bufferSize is buffer size for a websocket connection's read channel.
	readBuffSize = 128

	// The maximum time in seconds to write to a connection.
	writeWait = time.Second * 3

	// reconnectInterval is the initial and increment between reconnect tries.
	reconnectInterval = 5 * time.Second

	// maxReconnectInterval is the maximum allowed reconnect interval.
	maxReconnectInterval = time.Minute

	// DefaultResponseTimeout is the default timeout for responses after a
	// request is successfully sent.
	DefaultResponseTimeout = time.Minute
)

// ConnectionStatus represents the current status of the websocket connection.
type ConnectionStatus uint32

const (
	Disconnected ConnectionStatus = iota
	Connected
	InvalidCert
)

// String gives a human readable string for each connection status.
func (cs ConnectionStatus) String() string {
	switch cs {
	case Disconnected:
		return "disconnected"
	case Connected:
		return "connected"
	case InvalidCert:
		return "invalid certificate"
	default:
		return "unknown status"
	}
}

// invalidCertRegexp is a regexp that helps check for non-typed x509 errors
// caused by or related to an invalid cert.
var invalidCertRegexp = regexp.MustCompile(".*(unknown authority|not standards compliant|not trusted)")

// isErrorInvalidCert checks if the provided error is one of the different
// variant of an invalid cert error returned from the x509 package.
func isErrorInvalidCert(err error) bool {
	var invalidCertErr x509.CertificateInvalidError
	var unknownCertAuthErr x509.UnknownAuthorityError
	var hostNameErr x509.HostnameError
	return errors.As(err, &invalidCertErr) || errors.As(err, &hostNameErr) ||
		errors.As(err, &unknownCertAuthErr) || invalidCertRegexp.MatchString(err.Error())
}

// ErrInvalidCert is the error returned when attempting to use an invalid cert
// to set up a ws connection.
var ErrInvalidCert = fmt.Errorf("invalid certificate")

// ErrCertRequired is the error returned when a ws connection fails because no
// cert was provided.
var ErrCertRequired = fmt.Errorf("certificate required")

// WsConn is an interface for a websocket client.
type WsConn interface {
	NextID() uint64
	IsDown() bool
	Send(msg *msgjson.Message) error
	SendRaw(b []byte) error
	Request(msg *msgjson.Message, respHandler func(*msgjson.Message)) error
	RequestRaw(msgID uint64, rawMsg []byte, respHandler func(*msgjson.Message)) error
	RequestWithTimeout(msg *msgjson.Message, respHandler func(*msgjson.Message), expireTime time.Duration, expire func()) error
	Connect(ctx context.Context) (*sync.WaitGroup, error)
	MessageSource() <-chan *msgjson.Message
	UpdateURL(string)
}

// When the DEX sends a request to the client, a responseHandler is created
// to wait for the response.
type responseHandler struct {
	expiration *time.Timer
	f          func(*msgjson.Message)
	abort      func() // only to be run at most once, and not if f ran
}

// WsCfg is the configuration struct for initializing a WsConn.
type WsCfg struct {
	// URL is the websocket endpoint URL.
	URL string

	// The maximum time in seconds to wait for a ping from the server. This
	// should be larger than the server's ping interval to allow for network
	// latency.
	PingWait time.Duration

	// The server's certificate.
	Cert []byte

	// ReconnectSync runs the needed reconnection synchronization after
	// a reconnect.
	ReconnectSync func()

	// ConnectEventFunc runs whenever connection status changes.
	//
	// NOTE: Disconnect event notifications may lag behind actual
	// disconnections.
	ConnectEventFunc func(ConnectionStatus)

	// Logger is the logger for the WsConn.
	Logger dex.Logger

	// NetDialContext specifies an optional dialer context to use.
	NetDialContext func(context.Context, string, string) (net.Conn, error)

	// RawHandler overrides the msgjson parsing and forwards all messages to
	// the provided function.
	RawHandler func([]byte)

	// DisableAutoReconnect disables automatic reconnection.
	DisableAutoReconnect bool

	ConnectHeaders http.Header

	// EchoPingData will echo any data from pings as the pong data.
	EchoPingData bool
}

// wsConn represents a client websocket connection.
type wsConn struct {
	// 64-bit atomic variables first. See
	// https://golang.org/pkg/sync/atomic/#pkg-note-BUG.
	rID    uint64
	cancel context.CancelFunc
	wg     sync.WaitGroup
	log    dex.Logger
	cfg    *WsCfg
	tlsCfg *tls.Config
	readCh chan *msgjson.Message
	urlV   atomic.Value // string

	wsMtx sync.Mutex
	ws    *websocket.Conn

	connectionStatus uint32 // atomic

	reqMtx       sync.RWMutex
	respHandlers map[uint64]*responseHandler

	reconnectCh chan struct{} // trigger for immediate reconnect
}

var _ WsConn = (*wsConn)(nil)

// NewWsConn creates a client websocket connection.
func NewWsConn(cfg *WsCfg) (WsConn, error) {
	if cfg.PingWait < 0 {
		return nil, fmt.Errorf("ping wait cannot be negative")
	}

	uri, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("error parsing URL: %w", err)
	}

	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	if len(cfg.Cert) > 0 {
		if ok := rootCAs.AppendCertsFromPEM(cfg.Cert); !ok {
			return nil, ErrInvalidCert
		}
	}

	tlsConfig := &tls.Config{
		RootCAs:    rootCAs,
		MinVersion: tls.VersionTLS12,
		ServerName: uri.Hostname(),
	}

	conn := &wsConn{
		cfg:          cfg,
		log:          cfg.Logger,
		tlsCfg:       tlsConfig,
		readCh:       make(chan *msgjson.Message, readBuffSize),
		respHandlers: make(map[uint64]*responseHandler),
		reconnectCh:  make(chan struct{}, 1),
	}
	conn.urlV.Store(cfg.URL)

	return conn, nil
}

func (conn *wsConn) UpdateURL(uri string) {
	conn.urlV.Store(uri)
}

func (conn *wsConn) url() string {
	return conn.urlV.Load().(string)
}

// IsDown indicates if the connection is known to be down.
func (conn *wsConn) IsDown() bool {
	return atomic.LoadUint32(&conn.connectionStatus) != uint32(Connected)
}

// setConnectionStatus updates the connection's status and runs the
// ConnectEventFunc in case of a change.
func (conn *wsConn) setConnectionStatus(status ConnectionStatus) {
	oldStatus := atomic.SwapUint32(&conn.connectionStatus, uint32(status))
	statusChange := oldStatus != uint32(status)
	if statusChange && conn.cfg.ConnectEventFunc != nil {
		conn.cfg.ConnectEventFunc(status)
	}
}

// connect attempts to establish a websocket connection.
func (conn *wsConn) connect(ctx context.Context) error {
	dialer := &websocket.Dialer{
		HandshakeTimeout: DefaultResponseTimeout,
		TLSClientConfig:  conn.tlsCfg,
	}
	if conn.cfg.NetDialContext != nil {
		dialer.NetDialContext = conn.cfg.NetDialContext
	} else {
		dialer.Proxy = http.ProxyFromEnvironment
	}

	ws, _, err := dialer.DialContext(ctx, conn.url(), conn.cfg.ConnectHeaders)
	if err != nil {
		if isErrorInvalidCert(err) {
			conn.setConnectionStatus(InvalidCert)
			if len(conn.cfg.Cert) == 0 {
				return dex.NewError(ErrCertRequired, err.Error())
			}
			return dex.NewError(ErrInvalidCert, err.Error())
		}
		conn.setConnectionStatus(Disconnected)
		return err
	}

	// Set the initial read deadline for the first ping. Subsequent read
	// deadlines are set in the ping handler.
	err = ws.SetReadDeadline(time.Now().Add(conn.cfg.PingWait))
	if err != nil {
		conn.log.Errorf("set read deadline failed: %v", err)
		return err
	}

	echoPing := conn.cfg.EchoPingData

	ws.SetPingHandler(func(appData string) error {
		now := time.Now()

		// Set the deadline for the next ping.
		err := ws.SetReadDeadline(now.Add(conn.cfg.PingWait))
		if err != nil {
			conn.log.Errorf("set read deadline failed: %v", err)
			return err
		}

		var data []byte
		if echoPing {
			data = []byte(appData)
		}

		// Respond with a pong.
		err = ws.WriteControl(websocket.PongMessage, data, now.Add(writeWait))
		if err != nil {
			// read loop handles reconnect
			conn.log.Errorf("pong write error: %v", err)
			return err
		}

		return nil
	})

	conn.wsMtx.Lock()
	// If keepAlive called connect, the wsConn's current websocket.Conn may need
	// to be closed depending on the error that triggered the reconnect.
	if conn.ws != nil {
		conn.close()
	}
	conn.ws = ws
	conn.wsMtx.Unlock()

	conn.setConnectionStatus(Connected)
	conn.wg.Add(1)
	go func() {
		defer conn.wg.Done()
		if conn.cfg.RawHandler != nil {
			conn.readRaw(ctx)
		} else {
			conn.read(ctx)
		}
	}()

	return nil
}

func (conn *wsConn) SetReadLimit(limit int64) {
	conn.wsMtx.Lock()
	ws := conn.ws
	conn.wsMtx.Unlock()
	if ws != nil {
		ws.SetReadLimit(limit)
	}
}

func (conn *wsConn) handleReadError(err error) {
	reconnect := func() {
		conn.setConnectionStatus(Disconnected)
		if !conn.cfg.DisableAutoReconnect {
			conn.reconnectCh <- struct{}{}
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		conn.log.Errorf("Read timeout on connection to %s.", conn.url())
		reconnect()
		return
	}
	// TODO: Now that wsConn goroutines have contexts that are canceled
	// on shutdown, we do not have to infer the source and severity of
	// the error; just reconnect in ALL other cases, and remove the
	// following legacy checks.

	// Expected close errors (1000 and 1001) ... but if the server
	// closes we still want to reconnect. (???)
	if websocket.IsCloseError(err, websocket.CloseGoingAway,
		websocket.CloseNormalClosure) ||
		strings.Contains(err.Error(), "websocket: close sent") {
		reconnect()
		return
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "read" {
		if strings.Contains(opErr.Err.Error(), "use of closed network connection") {
			conn.log.Errorf("read quitting: %v", err)
			reconnect()
			return
		}
	}

	// Log all other errors and trigger a reconnection.
	conn.log.Errorf("read error (%v), attempting reconnection", err)
	reconnect()
}

func (conn *wsConn) close() {
	// Attempt to send a close message in case the connection is still live.
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")
	_ = conn.ws.WriteControl(websocket.CloseMessage, msg,
		time.Now().Add(50*time.Millisecond)) // ignore any error
	// Forcibly close the underlying connection.
	conn.ws.Close()
}

func (conn *wsConn) readRaw(ctx context.Context) {
	for {
		// Lock since conn.ws may be set by connect.
		conn.wsMtx.Lock()
		ws := conn.ws
		conn.wsMtx.Unlock()

		// Block until a message is received or an error occurs.
		_, msgBytes, err := ws.ReadMessage()
		// Drop the read error on context cancellation.
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			conn.handleReadError(err)
			return
		}
		conn.cfg.RawHandler(msgBytes)
	}
}

// read fetches and parses incoming messages for processing. This should be
// run as a goroutine. Increment the wg before calling read.
func (conn *wsConn) read(ctx context.Context) {
	for {
		msg := new(msgjson.Message)

		// Lock since conn.ws may be set by connect.
		conn.wsMtx.Lock()
		ws := conn.ws
		conn.wsMtx.Unlock()

		// The read itself does not require locking since only this goroutine
		// uses read functions that are not safe for concurrent use.
		err := ws.ReadJSON(msg)
		// Drop the read error on context cancellation.
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var mErr *json.UnmarshalTypeError
			if errors.As(err, &mErr) {
				// JSON decode errors are not fatal, log and proceed.
				conn.log.Errorf("json decode error: %v", mErr)
				continue
			}
			conn.handleReadError(err)
			return
		}

		// If the message is a response, find the handler.
		if msg.Type == msgjson.Response {
			handler := conn.respHandler(msg.ID)
			if handler == nil {
				b, _ := json.Marshal(msg)
				conn.log.Errorf("No handler found for response: %v", string(b))
				continue
			}
			// Run handlers in a goroutine so that other messages can be
			// received. Include the handler goroutines in the WaitGroup to
			// allow them to complete if the connection master desires.
			conn.wg.Add(1)
			go func() {
				defer conn.wg.Done()
				handler.f(msg)
			}()
			continue
		}
		conn.readCh <- msg
	}
}

// keepAlive maintains an active websocket connection by reconnecting when
// the established connection is broken. This should be run as a goroutine.
func (conn *wsConn) keepAlive(ctx context.Context) {
	rcInt := reconnectInterval
	for {
		select {
		case <-conn.reconnectCh:
			// Prioritize context cancellation even if there are reconnect
			// requests.
			if ctx.Err() != nil {
				return
			}

			conn.log.Infof("Attempting to reconnect to %s...", conn.url())
			err := conn.connect(ctx)
			if err != nil {
				conn.log.Errorf("Reconnect failed. Scheduling reconnect to %s in %.1f seconds.",
					conn.url(), rcInt.Seconds())
				time.AfterFunc(rcInt, func() {
					conn.reconnectCh <- struct{}{}
				})
				// Increment the wait up to PingWait.
				if rcInt < maxReconnectInterval {
					rcInt += reconnectInterval
				}
				continue
			}

			conn.log.Info("Successfully reconnected.")
			rcInt = reconnectInterval

			// Synchronize after a reconnection.
			if conn.cfg.ReconnectSync != nil {
				conn.cfg.ReconnectSync()
			}

		case <-ctx.Done():
			return
		}
	}
}

// NextID returns the next request id.
func (conn *wsConn) NextID() uint64 {
	return atomic.AddUint64(&conn.rID, 1)
}

// Connect connects the client. Any error encountered during the initial
// connection will be returned. An auto-(re)connect goroutine will be started,
// even on error. To terminate it, use Stop() or cancel the context.
func (conn *wsConn) Connect(ctx context.Context) (*sync.WaitGroup, error) {
	var ctxInternal context.Context
	ctxInternal, conn.cancel = context.WithCancel(ctx)

	err := conn.connect(ctxInternal)
	if err != nil {
		// If the certificate is invalid or missing, do not start the reconnect
		// loop, and return an error with no WaitGroup.
		if conn.cfg.DisableAutoReconnect || errors.Is(err, ErrInvalidCert) || errors.Is(err, ErrCertRequired) {
			conn.cancel()
			conn.wg.Wait() // probably a no-op
			close(conn.readCh)
			return nil, err
		}

		// The read loop would normally trigger keepAlive, but it wasn't started
		// on account of a connect error.
		conn.log.Errorf("Initial connection failed, starting reconnect loop: %v", err)
		time.AfterFunc(5*time.Second, func() {
			conn.reconnectCh <- struct{}{}
		})
	}

	if !conn.cfg.DisableAutoReconnect {
		conn.wg.Add(1)
		go func() {
			defer conn.wg.Done()
			conn.keepAlive(ctxInternal)
		}()
	}

	conn.wg.Add(1)
	go func() {
		defer conn.wg.Done()
		<-ctxInternal.Done()
		conn.setConnectionStatus(Disconnected)
		conn.wsMtx.Lock()
		if conn.ws != nil {
			conn.log.Debug("Sending close 1000 (normal) message.")
			conn.close()
		}
		conn.wsMtx.Unlock()

		// Run the expire funcs so request callers don't hang.
		conn.reqMtx.Lock()
		defer conn.reqMtx.Unlock()
		for id, h := range conn.respHandlers {
			delete(conn.respHandlers, id)
			// Since we are holding reqMtx and deleting the handler, no need to
			// check if expiration fired (see logReq), but good to stop it.
			h.expiration.Stop()
			h.abort()
		}

		close(conn.readCh) // signal to MessageSource receivers that the wsConn is dead
	}()

	return &conn.wg, nil
}

// Stop can be used to close the connection and all of the goroutines started by
// Connect. Alternatively, the context passed to Connect may be canceled.
func (conn *wsConn) Stop() {
	conn.cancel()
}

// Send pushes outgoing messages over the websocket connection. Sending of the
// message is synchronous, so a nil error guarantees that the message was
// successfully sent. A non-nil error may indicate that the connection is known
// to be down, the message failed to marshall to JSON, or writing to the
// websocket link failed.
func (conn *wsConn) Send(msg *msgjson.Message) error {
	if conn.IsDown() {
		return fmt.Errorf("cannot send on a broken connection")
	}

	// Marshal the Message first so that we don't send junk to the peer even if
	// it fails to marshal completely, which gorilla/websocket.WriteJSON does.
	b, err := json.Marshal(msg)
	if err != nil {
		conn.log.Errorf("Failed to marshal message: %v", err)
		return err
	}
	return conn.SendRaw(b)
}

// SendRaw sends a raw byte string over the websocket connection.
func (conn *wsConn) SendRaw(b []byte) error {
	if conn.IsDown() {
		return fmt.Errorf("cannot send on a broken connection")
	}

	conn.wsMtx.Lock()
	defer conn.wsMtx.Unlock()
	err := conn.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err != nil {
		conn.log.Errorf("Send: failed to set write deadline: %v", err)
		return err
	}

	err = conn.ws.WriteMessage(websocket.TextMessage, b)
	if err != nil {
		conn.log.Errorf("Send: WriteMessage error: %v", err)
		return err
	}
	return nil
}

// Request sends the Request-type msgjson.Message to the server and does not
// wait for a response, but records a callback function to run when a response
// is received. A response must be received within DefaultResponseTimeout of the
// request, after which the response handler expires and any late response will
// be ignored. To handle expiration or to set the timeout duration, use
// RequestWithTimeout. Sending of the request is synchronous, so a nil error
// guarantees that the request message was successfully sent.
func (conn *wsConn) Request(msg *msgjson.Message, f func(*msgjson.Message)) error {
	return conn.RequestWithTimeout(msg, f, DefaultResponseTimeout, func() {})
}

func (conn *wsConn) RequestRaw(msgID uint64, rawMsg []byte, f func(*msgjson.Message)) error {
	return conn.RequestRawWithTimeout(msgID, rawMsg, f, DefaultResponseTimeout, func() {})
}

// RequestWithTimeout sends the Request-type message and does not wait for a
// response, but records a callback function to run when a response is received.
// If the server responds within expireTime of the request, the response handler
// is called, otherwise the expire function is called. If the response handler
// is called, it is guaranteed that the response Message.ID is equal to the
// request Message.ID. Sending of the request is synchronous, so a nil error
// guarantees that the request message was successfully sent and that either the
// response handler or expire function will be run; a non-nil error guarantees
// that neither function will run.
//
// For example, to wait on a response or timeout:
//
//	errChan := make(chan error, 1)
//
//	err := conn.RequestWithTimeout(reqMsg, func(msg *msgjson.Message) {
//	    errChan <- msg.UnmarshalResult(responseStructPointer)
//	}, timeout, func() {
//	    errChan <- fmt.Errorf("timed out waiting for '%s' response.", route)
//	})
//	if err != nil {
//	    return err // request error
//	}
//	return <-errChan // timeout or response error
func (conn *wsConn) RequestWithTimeout(msg *msgjson.Message, f func(*msgjson.Message), expireTime time.Duration, expire func()) error {
	if msg.Type != msgjson.Request {
		return fmt.Errorf("Message is not a request: %v", msg.Type)
	}
	rawMsg, err := json.Marshal(msg)
	if err != nil {
		conn.log.Errorf("Failed to marshal message: %v", err)
		return err
	}
	err = conn.RequestRawWithTimeout(msg.ID, rawMsg, f, expireTime, expire)
	if err != nil {
		conn.log.Errorf("(*wsConn).Request(route '%s') Send error (%v), unregistering msg ID %d handler",
			msg.Route, err, msg.ID)
	}
	return err
}

func (conn *wsConn) RequestRawWithTimeout(msgID uint64, rawMsg []byte, f func(*msgjson.Message), expireTime time.Duration, expire func()) error {

	// Register the response and expire handlers for this request.
	conn.logReq(msgID, f, expireTime, expire)
	err := conn.SendRaw(rawMsg)
	if err != nil {
		// Neither expire nor the handler should run. Stop the expire timer
		// created by logReq and delete the response handler it added. The
		// caller receives a non-nil error to deal with it.
		conn.respHandler(msgID) // drop the responseHandler logged by logReq that is no longer necessary
	}
	return err
}

func (conn *wsConn) expire(id uint64) bool {
	conn.reqMtx.Lock()
	defer conn.reqMtx.Unlock()
	_, removed := conn.respHandlers[id]
	delete(conn.respHandlers, id)
	return removed
}

// logReq stores the response handler in the respHandlers map. Requests to the
// client are associated with a response handler.
func (conn *wsConn) logReq(id uint64, respHandler func(*msgjson.Message), expireTime time.Duration, expire func()) {
	conn.reqMtx.Lock()
	defer conn.reqMtx.Unlock()
	doExpire := func() {
		// Delete the response handler, and call the provided expire function if
		// (*wsLink).respHandler has not already retrieved the handler function
		// for execution.
		if conn.expire(id) {
			expire()
		}
	}
	conn.respHandlers[id] = &responseHandler{
		expiration: time.AfterFunc(expireTime, doExpire),
		f:          respHandler,
		abort:      expire,
	}
}

// respHandler extracts the response handler for the provided request ID if it
// exists, else nil. If the handler exists, it will be deleted from the map.
func (conn *wsConn) respHandler(id uint64) *responseHandler {
	conn.reqMtx.Lock()
	defer conn.reqMtx.Unlock()
	cb, ok := conn.respHandlers[id]
	if ok {
		cb.expiration.Stop()
		delete(conn.respHandlers, id)
	}
	return cb
}

// MessageSource returns the connection's read source. The returned chan will
// receive requests and notifications from the server, but not responses, which
// have handlers associated with their request. The same channel is returned on
// each call, so there must only be one receiver. When the connection is
// shutdown, the channel will be closed.
func (conn *wsConn) MessageSource() <-chan *msgjson.Message {
	return conn.readCh
}
