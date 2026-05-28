package server

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/semaphore"
)

// bridgePipelineDepth bounds the number of in-flight messages per
// direction in the pipelined bridge as a goroutine-handoff buffer.
// The dominant capacity bound is bridgeByteBudget; this channel
// depth only prevents pathological per-frame goroutine ping-pong
// for very small frames.
const bridgePipelineDepth = 1024

// bridgeByteBudget bounds the bytes in flight per direction in the
// pipelined bridge. Empirically measured against real Azure Relay
// rendezvous WebSockets: with a non-reading listener, Azure backs
// pressures the sender at ~14-16 MiB regardless of frame size
// (probed at 1 KiB, 32 KiB, and 256 KiB frame sizes). 12 MiB sits
// on the conservative side of that observed range so a test that
// passes against the mock will also pass against real Azure.
//
// var (not const) so unit tests can substitute a small budget to
// exercise backpressure without allocating large payloads.
var bridgeByteBudget int64 = 12 * 1024 * 1024

// bridgeWS bidirectionally copies WebSocket messages between two
// connections, preserving message boundaries and message types,
// while modelling end-to-end wire propagation as a pipelined delay
// of DelayProfile.SLatency + DelayProfile.LLatency in each direction.
//
// Why message boundaries matter: the aztunnel listener does a single
// ws.Read for the ConnectEnvelope (internal/listener/listener.go:72-80).
// If the server bridge coalesced that envelope with the first chunk of
// payload bytes into a single peer message, the listener's JSON
// unmarshal would either fail or silently consume the payload prefix.
// Each source message becomes exactly one peer message with the same
// message type.
//
// Termination: once either direction observes a clean close or error,
// the other side is unblocked via context cancellation and the WS is
// closed. The first error (if any) is returned. io.EOF and the
// websocket normal-close codes are folded into a nil return.
//
// Pipelined delay model: each direction is a reader + writer pair
// joined by a bounded channel. The reader stamps every message with
// an arriveBy time (now + S+L) and queues it; the writer sleeps until
// each message's arriveBy then forwards it. Multiple messages are
// in flight at once, ordering is preserved within a direction. This
// matches real-wire behaviour: a single echo round-trip pays
// 2*(S+L), but streaming N messages one-way pays one (S+L) total
// because TCP pipelines and the wire holds many bytes simultaneously
// (bandwidth-delay product). Stop-and-wait would have been wrong
// not just quantitatively (N×(S+L) vs S+L) but qualitatively (it
// would transform streams into stop-and-wait, including spurious
// backpressure that no real wire would produce).
func bridgeWS(ctx context.Context, p DelayProfile, a, b *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// End-to-end propagation: sender→listener = S+L; listener→sender =
	// L+S. Numerically equal in every preset we have, but kept as two
	// computations so a future asymmetric delay (e.g. only S applied
	// outbound from a half-duplex satellite link) would only require
	// changing one expression.
	abDelay := p.SLatency + p.LLatency
	baDelay := p.LLatency + p.SLatency

	errc := make(chan error, 2)
	go func() { errc <- pipelinedCopy(ctx, b, a, abDelay) }()
	go func() { errc <- pipelinedCopy(ctx, a, b, baDelay) }()

	first := <-errc
	cancel()
	// Force the other side to wake by closing both WSs. Use Close so
	// peers see a clean close frame.
	_ = a.Close(websocket.StatusNormalClosure, "bridge closing")
	_ = b.Close(websocket.StatusNormalClosure, "bridge closing")
	<-errc
	return foldCloseErr(first)
}

// queuedFrame is a single message waiting to be written by the
// pipelined bridge writer goroutine.
type queuedFrame struct {
	typ      websocket.MessageType
	data     []byte
	arriveBy time.Time
	weight   int64 // semaphore weight acquired for this frame
}

// bridgeFrameWeight returns the semaphore weight for a frame of n
// bytes, clamping to bridgeByteBudget. A single oversized frame
// acquires the entire budget and serialises behind any frames
// already queued; this matches real Azure, which has no observed
// per-frame size limit (probed up to 32 MiB) but caps aggregate
// in-flight bytes. (The mock itself imposes a separate defensive
// ceiling at the WS read layer via bridgeReadLimit in control.go,
// so frames larger than that are rejected before reaching here.)
// Reader and writer share this helper so the weight acquired and
// the weight released are guaranteed equal.
func bridgeFrameWeight(n int) int64 {
	w := int64(n)
	if w > bridgeByteBudget {
		return bridgeByteBudget
	}
	return w
}

// pipelinedCopy copies one direction of the bridge with a pipelined
// propagation delay. The reader stamps each message with its arrival
// time (now + delay), acquires byte-budget capacity, and pushes it
// onto a bounded channel; a writer goroutine drains the channel,
// sleeps until each message's arriveBy time, writes it to dst, and
// releases the byte-budget capacity.
//
// The byte budget models real Azure's aggregate per-direction buffer
// (see bridgeByteBudget). Note this is an approximation at the
// message-scheduling level: ws.Read returns whole messages, so the
// mock has fully read an oversized frame into memory before its
// weight blocks the next acquire — not the byte-level TCP
// backpressure mid-frame that real Azure exhibits.
//
// With delay=0 (DelayProfileZero), arriveBy is the read time,
// time.Until returns ~0, and the bridge is effectively instantaneous
// — the channel + writer-goroutine indirection adds only
// goroutine-scheduling noise.
func pipelinedCopy(ctx context.Context, dst, src *websocket.Conn, delay time.Duration) error {
	// Local context: cancelled when the writer exits so the reader is
	// promptly unblocked from Acquire or chan-send, instead of waiting
	// for the opposite bridge direction to tear down the WS.
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := semaphore.NewWeighted(bridgeByteBudget)
	ch := make(chan queuedFrame, bridgePipelineDepth)
	writerDone := make(chan error, 1)

	go func() {
		err := runPipelinedWriter(copyCtx, dst, ch, sem)
		cancel()
		writerDone <- err
	}()

	readErr := runPipelinedReader(copyCtx, src, ch, delay, sem)
	close(ch)

	// Bound the wait for the writer to drain in-flight frames after
	// the reader has finished. Under normal operation the writer is
	// constantly draining and `delay` is the worst-case wait for the
	// freshest queued frame's arriveBy. If the writer is parked on
	// a slow or dead destination peer, returning readErr promptly
	// lets bridgeWS cancel the other direction and close `dst`,
	// which unblocks the writer; the goroutine completes in the
	// background. Without this bound, a slow downstream peer would
	// hide the upstream peer's close from bridgeWS indefinitely.
	drainBudget := delay + 100*time.Millisecond
	drainTimer := time.NewTimer(drainBudget)
	defer drainTimer.Stop()
	select {
	case writeErr := <-writerDone:
		// Error precedence: a real readErr (read failure, ctx cancel
		// from the parent) is the root cause and wins. But if readErr
		// is context.Canceled AND the parent ctx is still live, the
		// cancellation must have been *induced* by the writer failing
		// and cancelling copyCtx (see runPipelinedWriter goroutine
		// above); in that case the writeErr is the real cause and the
		// readErr is just noise — prefer writeErr.
		if readErr != nil && errors.Is(readErr, context.Canceled) && ctx.Err() == nil && writeErr != nil {
			return writeErr
		}
		if readErr != nil {
			return readErr
		}
		return writeErr
	case <-drainTimer.C:
		return readErr
	}
}

// runPipelinedReader reads complete messages from src, acquires
// byte-budget capacity proportional to the message size, and queues
// the message for the writer with a stamped arriveBy time. It returns
// the first read or context error; the channel is closed by the
// caller. On Acquire failure (ctx cancellation) the budget is not
// consumed.
func runPipelinedReader(ctx context.Context, src *websocket.Conn, ch chan<- queuedFrame, delay time.Duration, sem *semaphore.Weighted) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		weight := bridgeFrameWeight(len(data))
		if err := sem.Acquire(ctx, weight); err != nil {
			return err
		}
		f := queuedFrame{
			typ:      typ,
			data:     data,
			arriveBy: time.Now().Add(delay),
			weight:   weight,
		}
		select {
		case ch <- f:
		case <-ctx.Done():
			sem.Release(weight)
			return ctx.Err()
		}
	}
}

// runPipelinedWriter drains the queue, sleeps until each message's
// arriveBy, then writes it to dst with the original message type.
// Each dequeued frame's weight is released via defer, so every exit
// path (delay-sleep cancellation, write error, normal completion)
// frees the byte budget — keeping release symmetric with acquire is
// required for liveness, otherwise the reader could remain blocked
// in Acquire after a writer failure.
//
// Exits when the channel is closed (clean) or on context cancellation
// (returns ctx.Err()).
func runPipelinedWriter(ctx context.Context, dst *websocket.Conn, ch <-chan queuedFrame, sem *semaphore.Weighted) error {
	for q := range ch {
		err := writePipelinedFrame(ctx, dst, q, sem)
		if err != nil {
			return err
		}
	}
	return nil
}

// writePipelinedFrame waits for q's arriveBy time, writes the frame to
// dst, and releases the semaphore weight via defer so every exit path
// frees the budget.
func writePipelinedFrame(ctx context.Context, dst *websocket.Conn, q queuedFrame, sem *semaphore.Weighted) error {
	defer sem.Release(q.weight)
	if d := time.Until(q.arriveBy); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return dst.Write(ctx, q.typ, q.data)
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
