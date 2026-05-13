package server

import (
	"context"
	"errors"
	"io"

	"github.com/coder/websocket"
)

// bridgeWS bidirectionally copies WebSocket messages between two
// connections, preserving message boundaries and message types.
//
// Why message boundaries matter: the aztunnel listener does a single
// ws.Read for the ConnectEnvelope (internal/listener/listener.go:72-80).
// If the server bridge coalesced that envelope with the first chunk of
// payload bytes into a single peer message, the listener's JSON
// unmarshal would either fail or silently consume the payload prefix.
// By calling ws.Reader once per source message and ws.Writer with the
// same message type, we guarantee each source message becomes exactly
// one peer message.
//
// Termination: once either direction observes a clean close or error,
// the other side is unblocked via context cancellation and the WS is
// closed. The first error (if any) is returned. io.EOF and the
// websocket normal-close codes are folded into a nil return.
func bridgeWS(ctx context.Context, a, b *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, 2)
	go func() { errc <- copyMessages(ctx, a, b) }()
	go func() { errc <- copyMessages(ctx, b, a) }()

	first := <-errc
	cancel()
	// Force the other side to wake by closing both WSs. Use Close so
	// peers see a clean close frame; if it has already closed,
	// CloseNow is a no-op-ish fallback.
	_ = a.Close(websocket.StatusNormalClosure, "bridge closing")
	_ = b.Close(websocket.StatusNormalClosure, "bridge closing")
	<-errc
	return foldCloseErr(first)
}

// copyMessages copies one direction of the bridge: read complete
// messages from src, write them to dst with the same message type.
func copyMessages(ctx context.Context, dst, src *websocket.Conn) error {
	for {
		typ, r, err := src.Reader(ctx)
		if err != nil {
			return err
		}
		w, err := dst.Writer(ctx, typ)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, r); err != nil {
			_ = w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
}

// foldCloseErr collapses expected closure errors (normal close, EOF) to
// nil so callers can treat them as success.
func foldCloseErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return nil
		}
	}
	return err
}
