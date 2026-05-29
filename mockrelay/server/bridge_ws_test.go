package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/semaphore"
)

func TestBridgeFrameWeight(t *testing.T) {
	// Modifies the package-level bridgeByteBudget; serialise with
	// other tests in this file that touch the same global.
	prev := bridgeByteBudget
	bridgeByteBudget = 4096
	defer func() { bridgeByteBudget = prev }()

	cases := []struct {
		name string
		n    int
		want int64
	}{
		{"zero", 0, 0},
		{"small", 100, 100},
		{"exact_budget", 4096, 4096},
		{"oversized_2x", 8192, 4096},
		{"oversized_huge", 1 << 30, 4096},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgeFrameWeight(tc.n); got != tc.want {
				t.Errorf("bridgeFrameWeight(%d) = %d, want %d", tc.n, got, tc.want)
			}
		})
	}
}

// wsPair returns a pair of connected websockets: the client end and
// the server end. The httptest server upgrades each request and
// publishes the server-side *websocket.Conn to serverCh, then blocks
// until the test calls shutdown. We deliberately do NOT read from
// the server-side conn in the handler — the test (or pipelinedCopy)
// owns the read pump.
type wsPair struct {
	clientWS *websocket.Conn
	serverWS *websocket.Conn
}

func newWSPairs(t *testing.T, ctx context.Context, n int) (pairs []wsPair, shutdown func()) {
	t.Helper()
	serverCh := make(chan *websocket.Conn, n)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("server-side Accept: %v", err)
			return
		}
		ws.SetReadLimit(1 << 30)
		serverCh <- ws
		// Hold the handler open so httptest doesn't tear down the
		// underlying TCP socket. Wait for either the per-test stop
		// channel or the underlying connection to drop.
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	pairs = make([]wsPair, n)
	for i := 0; i < n; i++ {
		client, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			close(stop)
			srv.Close()
			t.Fatalf("client Dial[%d]: %v", i, err)
		}
		client.SetReadLimit(1 << 30)
		var server *websocket.Conn
		select {
		case server = <-serverCh:
		case <-time.After(2 * time.Second):
			close(stop)
			srv.Close()
			t.Fatalf("server Accept[%d] timeout", i)
		}
		pairs[i] = wsPair{clientWS: client, serverWS: server}
	}
	shutdown = func() {
		close(stop)
		for _, p := range pairs {
			_ = p.clientWS.CloseNow()
			_ = p.serverWS.CloseNow()
		}
		srv.Close()
	}
	return pairs, shutdown
}

// TestPipelinedReader_BudgetBlocks confirms runPipelinedReader stops
// admitting frames once the byte budget is exhausted. With budget=4 KiB
// and 100 × 1 KiB frames from src, exactly 4 frames must be enqueued
// before Acquire blocks. Without the byte-budget semaphore the reader
// would drain all 100 frames into the channel.
func TestPipelinedReader_BudgetBlocks(t *testing.T) {
	prev := bridgeByteBudget
	bridgeByteBudget = 4 * 1024
	defer func() { bridgeByteBudget = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs, shutdown := newWSPairs(t, ctx, 1)
	defer shutdown()
	srcClient, srcServer := pairs[0].clientWS, pairs[0].serverWS

	// Channel capacity well above expected enqueue count so the chan
	// is never the binding constraint; the byte budget must be.
	ch := make(chan queuedFrame, 200)
	sem := semaphore.NewWeighted(bridgeByteBudget)

	readerCtx, readerCancel := context.WithCancel(ctx)
	defer readerCancel()
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- runPipelinedReader(readerCtx, srcServer, ch, 0, sem)
	}()

	payload := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		if err := srcClient.Write(ctx, websocket.MessageBinary, payload); err != nil {
			t.Fatalf("srcClient.Write[%d]: %v", i, err)
		}
	}

	// 200 ms is generous; on any sane CI the reader has either
	// enqueued ~4 frames and parked in Acquire, or it has misbehaved.
	time.Sleep(200 * time.Millisecond)

	wantEnqueued := int(bridgeByteBudget / int64(len(payload)))
	if got := len(ch); got != wantEnqueued {
		t.Fatalf("reader enqueued %d frames, want %d (budget=%d, frame=%d)",
			got, wantEnqueued, bridgeByteBudget, len(payload))
	}

	// Drain the channel and release the budget; the reader should
	// promptly admit another batch.
	for i := 0; i < wantEnqueued; i++ {
		f := <-ch
		sem.Release(f.weight)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(ch); got != wantEnqueued {
		t.Errorf("after release, reader enqueued %d more frames, want %d",
			got, wantEnqueued)
	}

	readerCancel()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("reader did not exit on cancel")
	}
}

// TestPipelinedReader_OversizedClampsAndBlocks confirms cap-and-serialise
// at the reader: an oversized frame is admitted alone with its weight
// clamped to bridgeByteBudget, and the next frame's Acquire blocks
// until the oversized frame's weight is released. Without the
// semaphore both frames would be enqueued immediately.
func TestPipelinedReader_OversizedClampsAndBlocks(t *testing.T) {
	prev := bridgeByteBudget
	bridgeByteBudget = 4 * 1024
	defer func() { bridgeByteBudget = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs, shutdown := newWSPairs(t, ctx, 1)
	defer shutdown()
	srcClient, srcServer := pairs[0].clientWS, pairs[0].serverWS

	ch := make(chan queuedFrame, 10)
	sem := semaphore.NewWeighted(bridgeByteBudget)

	readerCtx, readerCancel := context.WithCancel(ctx)
	defer readerCancel()
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- runPipelinedReader(readerCtx, srcServer, ch, 0, sem)
	}()

	oversized := make([]byte, 8*1024)
	followup := make([]byte, 1024)
	if err := srcClient.Write(ctx, websocket.MessageBinary, oversized); err != nil {
		t.Fatalf("srcClient.Write oversized: %v", err)
	}
	if err := srcClient.Write(ctx, websocket.MessageBinary, followup); err != nil {
		t.Fatalf("srcClient.Write followup: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if got := len(ch); got != 1 {
		t.Fatalf("enqueued %d frames before release, want 1 (followup must block)", got)
	}

	f := <-ch
	if f.weight != bridgeByteBudget {
		t.Errorf("oversized frame weight = %d, want %d (clamp to budget)",
			f.weight, bridgeByteBudget)
	}
	if len(f.data) != len(oversized) {
		t.Errorf("oversized data len = %d, want %d", len(f.data), len(oversized))
	}
	sem.Release(f.weight)

	time.Sleep(200 * time.Millisecond)
	if got := len(ch); got != 1 {
		t.Fatalf("enqueued %d frames after release, want 1 (followup)", got)
	}
	f2 := <-ch
	if f2.weight != int64(len(followup)) {
		t.Errorf("followup weight = %d, want %d", f2.weight, len(followup))
	}
	if len(f2.data) != len(followup) {
		t.Errorf("followup data len = %d, want %d", len(f2.data), len(followup))
	}
	sem.Release(f2.weight)

	readerCancel()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("reader did not exit on cancel")
	}
}

// TestRunPipelinedWriter_ReleasesOnError exercises the defer-release
// liveness invariant: when dst.Write fails, the writer must still
// release the dequeued frame's weight so the reader is not left
// blocked on Acquire. We pre-acquire half the budget (simulating the
// reader's acquire for the queued frame), enqueue a frame whose
// weight matches, close dst, run the writer, and assert that after
// it returns the full budget can be reacquired.
func TestRunPipelinedWriter_ReleasesOnError(t *testing.T) {
	prev := bridgeByteBudget
	bridgeByteBudget = 4 * 1024
	defer func() { bridgeByteBudget = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs, shutdown := newWSPairs(t, ctx, 1)
	defer shutdown()
	dstClient, dstServer := pairs[0].clientWS, pairs[0].serverWS

	sem := semaphore.NewWeighted(bridgeByteBudget)
	const queuedWeight int64 = 2048
	if !sem.TryAcquire(queuedWeight) {
		t.Fatalf("test setup: failed to pre-acquire %d from sem", queuedWeight)
	}

	// Close dst hard so the writer's ws.Write fails on first attempt.
	_ = dstClient.CloseNow()
	_ = dstServer.CloseNow()

	ch := make(chan queuedFrame, 1)
	ch <- queuedFrame{
		typ:      websocket.MessageBinary,
		data:     make([]byte, queuedWeight),
		arriveBy: time.Now(),
		weight:   queuedWeight,
	}
	close(ch)

	err := runPipelinedWriter(ctx, dstServer, ch, sem)
	if err == nil {
		t.Fatalf("expected error writing to closed conn, got nil")
	}

	// If the writer's defer-release fired, the full budget is now
	// available (pre-acquired 2048 was held; writer's defer freed
	// the queued 2048 on its error path). TryAcquire(budget) is the
	// sharp assertion: without defer-release the writer would have
	// returned without freeing the 2048, leaving only 2048 free.
	if !sem.TryAcquire(bridgeByteBudget) {
		t.Fatalf("writer did not release weight on error path; budget not fully available")
	}
}

// TestPipelinedCopy_WriterErrorReleasesBudget is an end-to-end smoke
// confirming pipelinedCopy as a whole exits promptly when dst is
// closed mid-stream, instead of leaving the reader parked in Acquire.
// The sharper internal release assertion lives in
// TestRunPipelinedWriter_ReleasesOnError; this test guards the
// outer-loop liveness.
func TestPipelinedCopy_WriterErrorReleasesBudget(t *testing.T) {
	prev := bridgeByteBudget
	bridgeByteBudget = 4096
	defer func() { bridgeByteBudget = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs, shutdown := newWSPairs(t, ctx, 2)
	defer shutdown()
	srcClient, srcServer := pairs[0].clientWS, pairs[0].serverWS
	dstClient, dstServer := pairs[1].clientWS, pairs[1].serverWS

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- pipelinedCopy(ctx, dstServer, srcServer, 0)
	}()

	// Send one frame and read it to confirm the bridge works.
	probe := make([]byte, 256)
	if err := srcClient.Write(ctx, websocket.MessageBinary, probe); err != nil {
		t.Fatalf("srcClient.Write probe: %v", err)
	}
	if _, _, err := dstClient.Read(ctx); err != nil {
		t.Fatalf("dstClient.Read probe: %v", err)
	}

	// Close dst hard so the writer's next ws.Write fails.
	_ = dstClient.CloseNow()
	_ = dstServer.CloseNow()

	// Pump frames into src; the bridge writer will error on its next
	// Write and pipelinedCopy should return promptly without hanging.
	// Even if the reader's Acquire briefly blocks on full budget,
	// the writer's failure cancels copyCtx (via the cancel() in
	// pipelinedCopy's writer goroutine wrapper), so the reader's
	// Acquire wakes with ctx err.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		payload := make([]byte, 1024)
		for i := 0; i < 50; i++ {
			if err := srcClient.Write(ctx, websocket.MessageBinary, payload); err != nil {
				return
			}
		}
	}()

	select {
	case <-copyDone:
		// Good: pipelinedCopy exited promptly.
	case <-time.After(3 * time.Second):
		t.Fatalf("pipelinedCopy did not exit after dst close")
	}
	<-pumpDone
}

// TestPipelinedCopy_PrefersWriterErrorOverInducedCancel asserts that
// when the writer fails (and cancels copyCtx, which makes the reader
// return context.Canceled), pipelinedCopy returns the underlying
// writer error rather than the induced context.Canceled noise. The
// parent ctx stays live throughout — only copyCtx is cancelled — so
// the precedence rule kicks in.
func TestPipelinedCopy_PrefersWriterErrorOverInducedCancel(t *testing.T) {
	prev := bridgeByteBudget
	bridgeByteBudget = 4096
	defer func() { bridgeByteBudget = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pairs, shutdown := newWSPairs(t, ctx, 2)
	defer shutdown()
	srcClient, srcServer := pairs[0].clientWS, pairs[0].serverWS
	dstClient, dstServer := pairs[1].clientWS, pairs[1].serverWS

	copyDone := make(chan error, 1)
	go func() {
		copyDone <- pipelinedCopy(ctx, dstServer, srcServer, 0)
	}()

	// Close dst BEFORE pumping so the writer's first ws.Write fails,
	// guaranteeing the writer-induced cancellation path.
	_ = dstClient.CloseNow()
	_ = dstServer.CloseNow()

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		payload := make([]byte, 1024)
		for i := 0; i < 50; i++ {
			if err := srcClient.Write(ctx, websocket.MessageBinary, payload); err != nil {
				return
			}
		}
	}()

	var got error
	select {
	case got = <-copyDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("pipelinedCopy did not exit")
	}
	<-pumpDone

	if got == nil {
		t.Fatalf("expected non-nil error, got nil")
	}
	// The parent ctx is still live (no timeout, no cancel), so a
	// context.Canceled return value would mean the writer error was
	// masked by the writer-induced copyCtx cancellation.
	if errors.Is(got, context.Canceled) && ctx.Err() == nil {
		t.Fatalf("got context.Canceled with parent ctx still live (%v); writer error was masked", got)
	}
}
