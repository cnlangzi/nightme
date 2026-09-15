package stt

import (
	"context"
	"io"
	"net"
	"sync"
)

// LoopbackTransport is an in-process Transport used by tests
// to exercise the client / server flow without touching the
// filesystem. Dial returns one end of a net.Pipe; Listen
// returns the other as the only accept-able connection.
//
// Not safe for concurrent use: LoopbackListener only ever
// holds one connection, matching how the worker behaves in V1
// (one transcription at a time).
type LoopbackTransport struct {
	listener *LoopbackListener
}

// Loopback returns a Transport + Listener pair bound together.
// Dial produces the client end; the Listener's Accept produces
// the server end. Bytes written by one end are readable by the
// other.
func Loopback() (*LoopbackTransport, *LoopbackListener) {
	ln := &LoopbackListener{
		pending: make(chan net.Conn, 1),
		closed:  make(chan struct{}),
	}
	return &LoopbackTransport{listener: ln}, ln
}

func (t *LoopbackTransport) Dial(ctx context.Context, endpoint Endpoint) (Conn, error) {
	if t.listener == nil {
		return nil, io.EOF
	}
	c1, c2 := net.Pipe()
	select {
	case t.listener.pending <- c1:
	case <-ctx.Done():
		c1.Close()
		c2.Close()
		return nil, ctx.Err()
	}
	return &loopbackConn{Conn: c2}, nil
}

func (t *LoopbackTransport) Listen(ctx context.Context, endpoint Endpoint) (Listener, error) {
	return t.listener, nil
}

// LoopbackListener is the server end of Loopback. Accept blocks
// until Dial is called on the matching transport.
type LoopbackListener struct {
	pending chan net.Conn
	closed  chan struct{}
	once    sync.Once
}

func (l *LoopbackListener) Endpoint() Endpoint { return "" }

func (l *LoopbackListener) Accept(ctx context.Context) (Conn, error) {
	select {
	case c := <-l.pending:
		return &loopbackConn{Conn: c}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, io.EOF
	}
}

func (l *LoopbackListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type loopbackConn struct {
	net.Conn
}

func (c loopbackConn) ReadFrame(ctx context.Context, out any) error {
	if err := ReadFrame(c.Conn, out); err != nil {
		return err
	}
	return nil
}

func (c loopbackConn) WriteFrame(ctx context.Context, in any) error {
	if err := WriteFrame(c.Conn, in); err != nil {
		return err
	}
	return nil
}

func (c loopbackConn) Close() error { return c.Conn.Close() }
