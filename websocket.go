package rewebsocket

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type state int

// ensure we implement io.Closer
var (
	_ io.Closer = &WebSocketTextClient{}
)

const (
	stateFresh state = iota
	stateOpen
	stateClosed
)

// WebSocketTextClient is a WebSocket text client that automatically reconnects
// to the remote service. All fields that you decide to set must be set before
// calling Open and are not safe to be modified after. When a connection
// problem occurs, writes will fail and some incoming messages may be lost. The
// client can only be opened and closed once.
type WebSocketTextClient struct {
	// OnReadMessage will be called for each incoming message. Messages will not
	// be processed concurrently unless the implementing function runs its logic
	// in a different goroutine. If this function blocks it will block the read
	// loop (which allows for flow control) and delay closing until it is
	// unblocked. If not set, messages will still be read but ignored. The given
	// cancel channel will be closed when the WebSocketTextClient is closed,
	// allowing the user to cancel long running operations.
	OnReadMessage func(cancel chan struct{}, msg []byte)

	// OnReopen will be called before reconnecting to obtain new parameters to
	// Open(). The operation will be retried using the Retry function. If not
	// set, the original values will be used. If this function blocks it will
	// halt reconnect and close progress. The given cancel channel will be closed
	// when the WebSocketTextClient is closed, allowing the user to cancel long
	// running operations.
	OnReopen func(cancel chan struct{}) (url string, header http.Header, err error)

	// debug logs
	logln func(...interface{})

	// OnError is called when there is a non-fatal error (typically failing to
	// read a message) This function will run in its own goroutine. If not set
	// the event will be lost.
	OnError func(error)

	// OnFatal is called when there is a fatal error (we can't reconnect after
	// retrying using the retry function).  When there is a fatal error the
	// client will be closed automatically. If not set the client will still
	// automatically close but the event will be lost. This function will run in
	// its own goroutine.
	OnFatal func(error)

	// Retry is a function that retries the given function until it gives up and
	// returns an error when the given channel is closed. It is used when
	// reconnecting. When the function returns an error, OnFatal will be called
	// and the client will be closed automatically. If not set reconnect
	// operations will only be attempted once. If this function blocks it will
	// halt reconnect progress. It will be called from a single goroutine.
	Retry func(chan struct{}, func() error) error

	// these two are set by Open() and then guarded by the reconnect loop
	url    string
	header http.Header

	// never reassigned after Open
	close       chan struct{}
	reconnectCh chan bool

	// guarded by connMutex
	conn      *websocket.Conn
	connMutex sync.RWMutex

	// guarded by stateMutex
	state      state
	stateMutex sync.Mutex
}

// Open opens the connection to the given URL and starts receiving messages
func (c *WebSocketTextClient) Open(url string, header http.Header) error {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	switch c.state {
	case stateClosed:
		return fmt.Errorf("already closed")
	case stateOpen:
		return fmt.Errorf("already open")
	}

	if c.logln == nil {
		c.logln = func(...interface{}) {}
	}

	if c.Retry == nil {
		c.Retry = func(done chan struct{}, f func() error) error {
			return f()
		}
	}

	if c.OnReadMessage == nil {
		c.OnReadMessage = func(chan struct{}, []byte) {}
	}

	if c.OnError == nil {
		c.OnError = func(error) {}
	}

	c.url = url
	c.header = header
	c.close = make(chan struct{})

	{
		conn, err := connect(url, header)
		if err != nil {
			return err
		}
		c.connMutex.Lock()
		c.conn = conn
		c.connMutex.Unlock()
	}

	// we rely on this being not buffered so only one reconnect can happen at a time
	c.reconnectCh = make(chan bool)
	go c.reconnectLoop()

	c.state = stateOpen

	go c.readLoop()

	return nil
}

// Close sends a close frame and then closes the underlying connection.
func (c *WebSocketTextClient) Close() error {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	if err := c.checkOpen(); err != nil {
		return err
	}

	c.state = stateClosed

	close(c.close)

	c.connMutex.RLock()
	defer c.connMutex.RUnlock()

	if err := c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(1*time.Second)); err != nil {
		// TODO(robbiev): preserve both errors?
		_ = c.conn.Close()
		return err
	}
	return c.conn.Close()
}

// WriteTextMessage writes a text message to the WebSocket
func (c *WebSocketTextClient) WriteTextMessage(msg []byte) error {
	c.connMutex.RLock()
	defer c.connMutex.RUnlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		c.tryReconnect()
		return err
	}
	return nil
}

// trigger a reconnect unless one is already in progress
func (c *WebSocketTextClient) tryReconnect() {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	if err := c.checkOpen(); err != nil {
		return
	}
	select {
	case c.reconnectCh <- true:
	default:
	}
}

// should run in its own goroutine
func (c *WebSocketTextClient) reconnectLoop() {
	for {
		select {
		case <-c.close:
			goto Exit
		case _ = <-c.reconnectCh:
			c.logln("reconnect")

			if c.OnReopen != nil {
				err := c.Retry(c.close, func() error {
					url, header, err := c.OnReopen(c.close)
					if err != nil {
						return err
					}
					c.url = url
					c.header = header
					return nil
				})

				if err != nil {
					go c.Close()
					if c.OnFatal != nil {
						go c.OnFatal(err)
					}
					goto Exit
				}
			}

			c.connMutex.RLock()
			_ = c.conn.Close()
			c.connMutex.RUnlock()

			var conn *websocket.Conn
			var err error

			err = c.Retry(c.close, func() error {
				conn, err = connect(c.url, c.header)
				return err
			})

			// TODO(robbiev): same code as above - deduplicate
			if err != nil {
				go c.Close()
				if c.OnFatal != nil {
					go c.OnFatal(err)
				}
				goto Exit
			}

			c.connMutex.Lock()
			c.conn = conn
			c.connMutex.Unlock()

			// if we closed in the meanwhile, disconnect
			c.stateMutex.Lock()
			if c.state == stateClosed {
				c.stateMutex.Unlock()
				goto Exit
			}
			go c.readLoop()
			c.stateMutex.Unlock()
		}
	}

Exit:
	c.logln("exit reconnect")
}

func (c *WebSocketTextClient) readLoop() {
	for {
		select {
		case <-c.close:
			return
		default:
			c.connMutex.RLock()
			msgType, msg, err := c.conn.ReadMessage()
			c.connMutex.RUnlock()
			if err != nil {
				go c.OnError(err)
				c.tryReconnect()
				return
			}
			if msgType != websocket.TextMessage {
				continue
			}

			// Note that the reason we don't send a reader (using gorilla's
			// NextReader) because it complicates processing; the reader should be
			// processed synchronously in the read loop because calling NextReader
			// again invalidates the previous reader. So then the first part of the
			// user's code must be synchronous but the rest can run in a goroutine,
			// which is easy to forget. Of course this costs us an extra allocation,
			// if this ever becomes a problem we can add an additional callback that
			// does take a reader and if set, only call that one.
			c.OnReadMessage(c.close, msg)
		}
	}
}

func connect(url string, header http.Header) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return nil, err
	}

	return conn, err
}

// need to take stateMutex when accessing this function
func (c *WebSocketTextClient) checkOpen() error {
	switch c.state {
	case stateClosed:
		return fmt.Errorf("already closed")
	case stateFresh:
		return fmt.Errorf("not open")
	}
	return nil
}
