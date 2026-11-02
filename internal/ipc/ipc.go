// Package ipc is the daemon's unix-socket protocol: one JSON object per line
// in each direction.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"time"

	"cccp/internal/core"
)

// Ops.
const (
	OpSnapshot  = "snapshot"  // reply: one Response with Snapshot
	OpSubscribe = "subscribe" // reply: a Response with Snapshot now and on every change
	OpRefresh   = "refresh"   // rescan every source; reply: Snapshot
	OpTmux      = "tmux"      // a tmux hook fired; no reply
	OpSeen      = "seen"      // mark Key seen now; reply: OK
	OpAlias     = "alias"     // set Value as alias for Key ("" clears); reply: OK
	OpReload    = "reload"    // reread the config file; reply: OK
)

// Request is a client message.
type Request struct {
	Op     string `json:"op"`
	Event  string `json:"event,omitempty"`
	Pane   string `json:"pane,omitempty"`
	Client string `json:"client,omitempty"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value,omitempty"`
}

// Response is a daemon message.
type Response struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Snapshot *core.Snapshot `json:"snapshot,omitempty"`
}

// ErrNoDaemon means nothing is listening on the socket.
var ErrNoDaemon = errors.New("daemon not running")

// Client talks to the daemon listening on Socket.
type Client struct {
	Socket string
}

// Dial connects to the daemon.
func (cl Client) Dial(timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", cl.Socket, timeout)
	if err != nil {
		return nil, ErrNoDaemon
	}
	return c, nil
}

// Send sends one request without waiting for a reply.
func (cl Client) Send(req Request, timeout time.Duration) error {
	c, err := cl.Dial(timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	return json.NewEncoder(c).Encode(req)
}

// Call sends a request and reads one response.
func (cl Client) Call(req Request, timeout time.Duration) (Response, error) {
	c, err := cl.Dial(timeout)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&resp); err != nil {
		return Response{}, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Subscription streams snapshots from the daemon.
type Subscription struct {
	conn net.Conn
	dec  *json.Decoder
}

// Subscribe opens a subscription.
func (cl Client) Subscribe(timeout time.Duration) (*Subscription, error) {
	c, err := cl.Dial(timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetWriteDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(c).Encode(Request{Op: OpSubscribe}); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetWriteDeadline(time.Time{})
	return &Subscription{conn: c, dec: json.NewDecoder(bufio.NewReaderSize(c, 1<<20))}, nil
}

// Next blocks for the next snapshot.
func (s *Subscription) Next() (core.Snapshot, error) {
	var r Response
	if err := s.dec.Decode(&r); err != nil {
		return core.Snapshot{}, err
	}
	if r.Snapshot == nil {
		return core.Snapshot{}, errors.New("subscription message without snapshot")
	}
	return *r.Snapshot, nil
}

// Close ends the subscription.
func (s *Subscription) Close() error { return s.conn.Close() }
