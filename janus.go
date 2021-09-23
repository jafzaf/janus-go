// Package janus is a Golang implementation of the Janus API, used to interact
// with the Janus WebRTC Gateway.
package janus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
	"nhooyr.io/websocket"
)

// The message types are defined in RFC 6455, section 11.8.
const (
	pingMessage     = 9
	timeoutDuration = 1 * time.Second
)

func unexpected(request string) error {
	return fmt.Errorf("Unexpected response received to '%s' request", request)
}

func newRequest(method string) (map[string]interface{}, chan interface{}) {
	req := make(map[string]interface{}, 8)
	req["janus"] = method
	return req, make(chan interface{})
}

// Gateway represents a connection to an instance of the Janus Gateway.
type Gateway struct {
	// Sessions is a map of the currently active sessions to the gateway.
	Sessions map[uint64]*Session

	// Access to the Sessions map should be synchronized with the Gateway.Lock()
	// and Gateway.Unlock() methods provided by the embeded sync.Mutex.
	sync.Mutex

	conn             *websocket.Conn
	transactions     map[xid.ID]chan interface{}
	transactionsUsed map[xid.ID]bool
	apiSecret	 string

	// LogJsonMessages enables logging of json rx/tx messages to stdout
	LogJsonMessages bool
}

func generateTransactionId() xid.ID {
	return xid.New()
}

// Connect initiates a websock connection with the Janus Gateway
//
// It will also spawn two goroutines to maintain the connection
// One is for sending Websocket ping messages periodically
// The other is for reading messages and passing to the right channel
//
// These two goroutines are added to an errgroup.Group
// which you can ignore if you like: gateway, _, err := janus.Connect
// or even better you can use the Wait() method to wait for these
// methods, AND CATCH ANY ERRORS THAT OCCUR inside of them.
// The readme has links to more info on errgroup.
//
func Connect(ctx context.Context, wsURL string, secret string) (*Gateway, *errgroup.Group, error) {

	opts := &websocket.DialOptions{Subprotocols: []string{"janus-protocol"}}
	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return nil, nil, err
	}

	gateway := new(Gateway)
	gateway.conn = conn
	gateway.transactions = make(map[xid.ID]chan interface{})
	gateway.transactionsUsed = make(map[xid.ID]bool)
	gateway.Sessions = make(map[uint64]*Session)
	gateway.apiSecret = secret

	// By returning errgroup.Group callers can wait for
	// any errors that occur in these goroutines. Powerful.
	// by simplying calling Wait() on the group.
	// also, if either of these goroutines return an error,
	// the other one will be cancelled.
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return gateway.ping(ctx) })
	g.Go(func() error { return gateway.recv(ctx) })

	return gateway, g, nil
}

// WaitForGroup is an example of
// wait and catch errors from the Connect() function
func WaitForGroup(g *errgroup.Group) error {
	err := g.Wait() //finish when
	if err != nil {
		println(fmt.Sprintf("janus-session ended with error %v", err)) //stderr
	}
	return err
}

// Close closes the underlying connection to the Gateway.
func (gateway *Gateway) Close(code websocket.StatusCode, reason string) error {
	return gateway.conn.Close(code, reason)
}

func (gateway *Gateway) send(ctx context.Context, msg map[string]interface{}, transaction chan interface{}) error {
	guid := generateTransactionId()

	msg["transaction"] = guid.String()
	if gateway.apiSecret != "" {
		msg["apisecret"] = gateway.apiSecret
	}
	gateway.Lock()
	gateway.transactions[guid] = transaction
	gateway.transactionsUsed[guid] = false
	gateway.Unlock()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	if gateway.LogJsonMessages {
		// log message being sent
		var log bytes.Buffer
		_ = json.Indent(&log, data, ">", "   ")
		log.Write([]byte("\n"))
		_, _ = log.WriteTo(os.Stdout)
	}

	err = gateway.conn.Write(ctx, websocket.MessageText, data)
	return err

}

func passMsg(ch chan interface{}, msg interface{}) {
	select {
	case ch <- msg:
	case <-time.After(timeoutDuration):
		println("no reader/discarded %#v", msg)
	}
}

// ping should be started as a goroutine,
// it will periodically send a ping over the wire
// to keep Janus happy with the gateway connection
//
func (gateway *Gateway) ping(ctx context.Context) error {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			//fmt.Println("wsping on websock")
			err := gateway.conn.Write(ctx, pingMessage, []byte{})
			if err != nil {
				return err
			}
		}
	}
}

// KeepAliveSender will send keepalive messages every 20 seconds
// for the session
func (session *Session) KeepAliveSender(ctx context.Context) error {
	// regarding ticker, you need to ping janus at least every 60 seconds
	// https://janus.conf.meetecho.com/docs/rest.html#WS
	ticker := time.NewTicker(time.Second * 20)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, err := session.KeepAlive(ctx)
			if err != nil {
				return err
			}
		}
	}
}

// recv should be started as a goroutine
func (gateway *Gateway) recv(ctx context.Context) error {

	for {
		// Read message from Gateway

		// Decode to Msg struct
		var base BaseMsg

		_, data, err := gateway.conn.Read(ctx)
		if err != nil {
			return err
		}

		if err := json.Unmarshal(data, &base); err != nil {
			return err
		}

		if gateway.LogJsonMessages {
			// log message being sent
			var log bytes.Buffer
			_ = json.Indent(&log, data, "<", "   ")
			log.Write([]byte("\n"))
			_, _ = log.WriteTo(os.Stdout)
		}

		typeFunc, ok := msgtypes[base.Type]
		if !ok {
			fmt.Printf("Unknown message type received!\n")
			// 122220 use continue, not return error.
			// hopefully best trade-off between fail-early
			// robust run-time behavior
			continue
		}

		msg := typeFunc()
		if err := json.Unmarshal(data, &msg); err != nil {
			fmt.Printf("json.Unmarshal: %s\n", err)
			// 122220 change from continue to return err
			// if this happens, it probably means we have a serious error
			return err
		}

		var transactionUsed bool
		if base.ID != "" {
			id, _ := xid.FromString(base.ID)
			gateway.Lock()
			transactionUsed = gateway.transactionsUsed[id]
			gateway.Unlock()
		}

		// Pass message on from here
		if base.ID == "" || transactionUsed {
			// Is this a Handle event?
			if base.Handle == 0 {
				// Error()
			} else {
				// Lookup Session
				gateway.Lock()
				session := gateway.Sessions[base.Session]
				gateway.Unlock()
				if session == nil {
					fmt.Printf("Unable to deliver message. Session gone?\n")
					// 122220 leave as continue, not return err
					continue
				}

				// Lookup Handle
				session.Lock()
				handle := session.Handles[base.Handle]
				session.Unlock()
				if handle == nil {
					fmt.Printf("Unable to deliver message. Handle gone?\n")
					// 122220 leave as continue, not return err
					continue
				}

				// Pass msg
				go passMsg(handle.Events, msg)
			}
		} else {
			id, _ := xid.FromString(base.ID)
			// Lookup Transaction
			gateway.Lock()
			transaction := gateway.transactions[id]
			switch msg.(type) {
			case *EventMsg:
				gateway.transactionsUsed[id] = true
			}
			gateway.Unlock()
			if transaction == nil {
				return fmt.Errorf("null transaction")
			}

			// Pass msg
			go passMsg(transaction, msg)
		}
	}
}

// Info sends an info request to the Gateway.
// On success, an InfoMsg will be returned and error will be nil.
func (gateway *Gateway) Info(ctx context.Context) (*InfoMsg, error) {
	req, ch := newRequest("info")
	err := gateway.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'info'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *InfoMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("info")
}

// Create sends a create request to the Gateway.
// On success, a new Session will be returned and error will be nil.
func (gateway *Gateway) Create(ctx context.Context) (*Session, error) {
	req, ch := newRequest("create")
	err := gateway.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	var success *SuccessMsg
	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'create'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *SuccessMsg:
			success = msg
		case *ErrorMsg:
			return nil, msg
		}
	}

	// Create new session
	session := new(Session)
	session.gateway = gateway
	session.ID = success.Data.ID
	session.Handles = make(map[uint64]*Handle)
	session.Events = make(chan interface{}, 2)

	// Store this session
	gateway.Lock()
	gateway.Sessions[session.ID] = session
	gateway.Unlock()

	return session, nil
}

// Session represents a session instance on the Janus Gateway.
type Session struct {
	// ID is the session_id of this session
	ID uint64

	// Handles is a map of plugin handles within this session
	Handles map[uint64]*Handle

	Events chan interface{}

	// Access to the Handles map should be synchronized with the Session.Lock()
	// and Session.Unlock() methods provided by the embeded sync.Mutex.
	sync.Mutex

	gateway *Gateway
}

func (session *Session) send(ctx context.Context, msg map[string]interface{}, transaction chan interface{}) error {
	msg["session_id"] = session.ID
	return session.gateway.send(ctx, msg, transaction)
}

// Attach sends an attach request to the Gateway within this session.
// plugin should be the unique string of the plugin to attach to.
// On success, a new Handle will be returned and error will be nil.
func (session *Session) Attach(ctx context.Context, plugin string) (*Handle, error) {
	req, ch := newRequest("attach")
	req["plugin"] = plugin
	err := session.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	var success *SuccessMsg
	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'attach'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *SuccessMsg:
			success = msg
		case *ErrorMsg:
			return nil, msg
		}
	}

	handle := new(Handle)
	handle.session = session
	handle.ID = success.Data.ID
	handle.Events = make(chan interface{}, 8)

	session.Lock()
	session.Handles[handle.ID] = handle
	session.Unlock()

	return handle, nil
}

// KeepAlive sends a keep-alive request to the Gateway.
// On success, an AckMsg will be returned and error will be nil.
func (session *Session) KeepAlive(ctx context.Context) (*AckMsg, error) {
	req, ch := newRequest("keepalive")
	err := session.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'keepalive'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("keepalive")
}

// Destroy sends a destroy request to the Gateway to tear down this session.
// On success, the Session will be removed from the Gateway.Sessions map, an
// AckMsg will be returned and error will be nil.
func (session *Session) Destroy(ctx context.Context) (*AckMsg, error) {
	req, ch := newRequest("destroy")
	err := session.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	var ack *AckMsg
	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'destroy'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			ack = msg
		case *ErrorMsg:
			return nil, msg
		}
	}

	// Remove this session from the gateway
	session.gateway.Lock()
	delete(session.gateway.Sessions, session.ID)
	session.gateway.Unlock()

	return ack, nil
}

// Handle represents a handle to a plugin instance on the Gateway.
type Handle struct {
	// ID is the handle_id of this plugin handle
	ID uint64

	// Type   // pub  or sub
	Type string

	//User   // Userid
	User string

	// Events is a receive only channel that can be used to receive events
	// related to this handle from the gateway.
	Events chan interface{}

	session *Session
}

func (handle *Handle) send(ctx context.Context, msg map[string]interface{}, transaction chan interface{}) error {
	msg["handle_id"] = handle.ID
	err := handle.session.send(ctx, msg, transaction)
	return err

}

// Request sends a sync request
func (handle *Handle) Request(ctx context.Context, body interface{}) (*SuccessMsg, error) {
	req, ch := newRequest("message")
	if body != nil {
		req["body"] = body
	}
	err := handle.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'message'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *SuccessMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("message")
}

// Message sends a message request to a plugin handle on the Gateway.
// body should be the plugin data to be passed to the plugin, and jsep should
// contain an optional SDP offer/answer to establish a WebRTC PeerConnection.
// On success, an EventMsg will be returned and error will be nil.
func (handle *Handle) Message(ctx context.Context, body, jsep interface{}) (*EventMsg, error) {
	req, ch := newRequest("message")
	if body != nil {
		req["body"] = body
	}
	if jsep != nil {
		req["jsep"] = jsep
	}
	err := handle.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

GetMessage: // No tears..

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'message'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			goto GetMessage // ..only dreams.
		case *EventMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("message")
}

// Trickle sends a trickle request to the Gateway as part of establishing
// a new PeerConnection with a plugin.
// candidate should be a single ICE candidate, or a completed object to
// signify that all candidates have been sent:
//		{
//			"completed": true
//		}
// On success, an AckMsg will be returned and error will be nil.
func (handle *Handle) Trickle(ctx context.Context, candidate interface{}) (*AckMsg, error) {
	req, ch := newRequest("trickle")
	req["candidate"] = candidate
	err := handle.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'trickle'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("trickle")
}

// TrickleMany sends a trickle request to the Gateway as part of establishing
// a new PeerConnection with a plugin.
// candidates should be an array of ICE candidates.
// On success, an AckMsg will be returned and error will be nil.
func (handle *Handle) TrickleMany(ctx context.Context, candidates interface{}) (*AckMsg, error) {
	req, ch := newRequest("trickle")
	req["candidates"] = candidates
	err := handle.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'trickle'/many")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			return msg, nil
		case *ErrorMsg:
			return nil, msg
		}
	}

	return nil, unexpected("trickle")
}

// Detach sends a detach request to the Gateway to remove this handle.
// On success, an AckMsg will be returned and error will be nil.
func (handle *Handle) Detach(ctx context.Context) (*AckMsg, error) {
	req, ch := newRequest("detach")
	err := handle.send(ctx, req, ch)
	if err != nil {
		return nil, err
	}

	var ack *AckMsg
	select {
	case <-time.After(timeoutDuration):
		return nil,fmt.Errorf("timeout waiting for response to 'detach'")
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg := <-ch:
		switch msg := msg.(type) {
		case *AckMsg:
			ack = msg
		case *ErrorMsg:
			return nil, msg
		}
	}

	// Remove this handle from the session
	handle.session.Lock()
	delete(handle.session.Handles, handle.ID)
	handle.session.Unlock()

	return ack, nil
}
