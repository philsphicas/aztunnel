package scenarios

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RunBenchmarks registers every e2e benchmark as a sub-bench under
// b. Mirrors RunCoreScenarios for tests: a single entry point keeps the
// per-backend bench_test.go files trivial.
//
// The sub-benches measure the connection-setup and concurrent-
// connect dimensions of the relay path, plus a control benchmark
// (SteadyThroughput) that should NOT regress under architectural
// changes. Run with `-count=5 -benchmem` and feed both outputs into
// benchstat to quantify the change.
//
// Each sub-bench stands up its own topology via backend.Setup(b, ...)
// and tears it down through t.Cleanup. AssertNoLeaks is deliberately
// not called: leak polling inside a benchmark would inflate the
// timer.
func RunBenchmarks(b *testing.B, backend Backend) {
	b.Helper()
	b.Run("ConnectLatency_Serial_PortForward", func(b *testing.B) {
		benchConnectLatencySerial(b, backend, ModePortForward)
	})
	b.Run("ConnectLatency_Serial_SOCKS5", func(b *testing.B) {
		benchConnectLatencySerial(b, backend, ModeSOCKS5)
	})
	b.Run("ConcurrentConnect_N100", func(b *testing.B) {
		benchConcurrentConnect(b, backend, 100)
	})
	b.Run("ShortSessionRate_N1", func(b *testing.B) {
		benchShortSessionRate(b, backend, 1)
	})
	b.Run("ShortSessionRate_N2", func(b *testing.B) {
		benchShortSessionRate(b, backend, 2)
	})
	b.Run("SteadyThroughput", func(b *testing.B) {
		benchSteadyThroughput(b, backend)
	})
	b.Run("ConnectFailureRate", func(b *testing.B) {
		benchConnectFailureRate(b, backend)
	})
}

// benchConnectLatencySerial measures the per-iteration wall time of:
//
//	Dial → write 1 byte → read 1 byte echoed back → close.
//
// Single connection per iteration, no concurrency. The headline
// connect-latency metric: each iteration pays one full relay
// rendezvous round-trip (~1–2 s on Azure today).
//
// Run against both modes (port-forward, SOCKS5) because the SOCKS5
// path adds a per-connection handshake that may carry different
// setup costs than raw port-forward.
func benchConnectLatencySerial(b *testing.B, backend Backend, mode SenderMode) {
	b.Helper()
	echo := StartPlainEcho(b)
	opts := SetupOptions{
		NumListeners:   1,
		SenderMode:     mode,
		AllowedTargets: []string{echo.Addr()},
	}
	if mode == ModePortForward {
		opts.Target = echo.Addr()
	}
	tun := backend.Setup(b, opts)

	payload := []byte{0x42}
	buf := make([]byte, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := benchDial(tun.SenderAddr, echo.Addr(), mode, 30*time.Second)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		if err := writeFull(conn, payload); err != nil {
			_ = conn.Close()
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			_ = conn.Close()
			b.Fatalf("read: %v", err)
		}
		if err := conn.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}

// benchConcurrentConnect measures the wall time of opening n
// concurrent connections, each of which round-trips 1 byte, per
// iteration. Surfaces the concurrency-amplified setup cost: when
// flows share a single listener they queue at rendezvous; lower
// results indicate connection setup can fan out cheaply instead.
//
// Each iteration owns n goroutines; b.N controls how many iterations
// run. Worker errors are collected and surfaced from the benchmark
// goroutine (b.Fatalf from a worker is racy).
func benchConcurrentConnect(b *testing.B, backend Backend, n int) {
	b.Helper()
	skipIfFDLimitTooLow(b, n*8+64)
	echo := StartPlainEcho(b)
	tun := backend.Setup(b, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		errs := make(chan error, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for j := 0; j < n; j++ {
			go func() {
				defer wg.Done()
				conn, err := net.DialTimeout("tcp", tun.SenderAddr, 60*time.Second)
				if err != nil {
					errs <- fmt.Errorf("dial: %w", err)
					return
				}
				defer conn.Close() //nolint:errcheck // best-effort
				_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
				if err := writeFull(conn, []byte{0x42}); err != nil {
					errs <- fmt.Errorf("write: %w", err)
					return
				}
				buf := make([]byte, 1)
				if _, err := io.ReadFull(conn, buf); err != nil {
					errs <- fmt.Errorf("read: %w", err)
					return
				}
			}()
		}
		wg.Wait()
		close(errs)
		if err := drainErrors(errs); err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// benchShortSessionRate measures ops/sec of the
// open → write 1 KB → read 1 KB → close cycle. The canonical
// short-session workload — sustained rate of fresh connections,
// each carrying a small payload.
//
// numSenders controls how many sender binds receive the load round-
// robin. The single-sender variant is the strict baseline; the
// multi-sender variant probes whether scaling out senders compensates
// for serial per-sender rendezvous.
//
// b.SetBytes(1024) makes MB/s appear alongside ns/op for benchstat,
// counting payload bytes written per iteration.
func benchShortSessionRate(b *testing.B, backend Backend, numSenders int) {
	b.Helper()
	echo := StartPlainEcho(b)
	tun := backend.Setup(b, SetupOptions{
		NumListeners:   1,
		NumSenders:     numSenders,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	const payloadSize = 1024
	payload := make([]byte, payloadSize)
	if _, err := rand.Read(payload); err != nil {
		b.Fatalf("rand: %v", err)
	}

	b.SetBytes(int64(payloadSize))
	b.ReportAllocs()
	b.ResetTimer()
	buf := make([]byte, payloadSize)
	for i := 0; i < b.N; i++ {
		addr := tun.SenderAddrs[i%len(tun.SenderAddrs)]
		conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := writeFull(conn, payload); err != nil {
			_ = conn.Close()
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			_ = conn.Close()
			b.Fatalf("read: %v", err)
		}
		if err := conn.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}

// benchSteadyThroughput measures MB/s through a single established
// connection over a 1 MB bidirectional transfer per iteration. The
// connection is dialed once before the timer starts and reused for
// every iteration so setup cost is excluded.
//
// This is the control benchmark: it should NOT regress under
// architectural changes. Writes and reads run concurrently to avoid
// socket-buffer deadlock, and each iteration drains exactly
// transferSize bytes before starting the next.
func benchSteadyThroughput(b *testing.B, backend Backend) {
	b.Helper()
	echo := StartPlainEcho(b)
	tun := backend.Setup(b, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	conn, err := net.DialTimeout("tcp", tun.SenderAddr, 30*time.Second)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort

	// Warmup: drive the relay rendezvous before measurement.
	// net.DialTimeout only completes the local TCP accept; the sender
	// dials the relay lazily on first data, which on Azure adds ~1s
	// to iteration 0 unless we flush a probe through.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write([]byte{0}); err != nil {
		b.Fatalf("warmup write: %v", err)
	}
	var warmupBuf [1]byte
	if _, err := io.ReadFull(conn, warmupBuf[:]); err != nil {
		b.Fatalf("warmup read: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})

	const transferSize = 1024 * 1024
	body := make([]byte, transferSize)
	if _, err := rand.Read(body); err != nil {
		b.Fatalf("rand: %v", err)
	}
	readBuf := make([]byte, transferSize)

	b.SetBytes(int64(transferSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- writeFull(conn, body)
		}()
		if _, err := io.ReadFull(conn, readBuf); err != nil {
			b.Fatalf("iter %d read: %v", i, err)
		}
		if err := <-writeDone; err != nil {
			b.Fatalf("iter %d write: %v", i, err)
		}
	}
}

// benchConnectFailureRate measures stability under load. Each
// iteration fires numDials=500 concurrent dials and counts how many
// errored. The aggregate failure fraction across all iterations is
// reported via b.ReportMetric so benchstat can diff it.
//
// The default ns/op reflects the wall time of one 500-dial round and
// is preserved for completeness; the headline signal is the custom
// fail% metric.
func benchConnectFailureRate(b *testing.B, backend Backend) {
	b.Helper()
	const numDials = 500
	skipIfFDLimitTooLow(b, numDials*4+64)
	echo := StartPlainEcho(b)
	tun := backend.Setup(b, SetupOptions{
		NumListeners:   1,
		SenderMode:     ModePortForward,
		Target:         echo.Addr(),
		AllowedTargets: []string{echo.Addr()},
	})

	var totalFailures atomic.Int64
	var totalDials atomic.Int64

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(numDials)
		for j := 0; j < numDials; j++ {
			go func() {
				defer wg.Done()
				totalDials.Add(1)
				conn, err := net.DialTimeout("tcp", tun.SenderAddr, 30*time.Second)
				if err != nil {
					totalFailures.Add(1)
					return
				}
				// Issue a tiny round-trip so a successful TCP that
				// then fails inside the bridge is still counted as a
				// failure. Without this, a sender that accepts the
				// TCP and immediately drops would look like a
				// success.
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				if err := writeFull(conn, []byte{0x42}); err != nil {
					totalFailures.Add(1)
					_ = conn.Close()
					return
				}
				buf := make([]byte, 1)
				if _, err := io.ReadFull(conn, buf); err != nil {
					totalFailures.Add(1)
					_ = conn.Close()
					return
				}
				_ = conn.Close()
			}()
		}
		wg.Wait()
	}
	b.StopTimer()

	dials := totalDials.Load()
	fails := totalFailures.Load()
	var pct float64
	if dials > 0 {
		pct = float64(fails) / float64(dials) * 100.0
	}
	b.ReportMetric(pct, "fail%")
}

// benchDial opens a connection to the sender bind for the given
// mode. For SOCKS5 it performs the CONNECT handshake to the supplied
// target; for port-forward it returns the raw TCP connection.
func benchDial(senderAddr, target string, mode SenderMode, timeout time.Duration) (net.Conn, error) {
	switch mode {
	case ModePortForward:
		return net.DialTimeout("tcp", senderAddr, timeout)
	case ModeSOCKS5:
		return DialSOCKS5(senderAddr, target, timeout)
	default:
		return nil, fmt.Errorf("unknown SenderMode %v", mode)
	}
}

// skipIfFDLimitTooLow skips the benchmark when the soft FD limit is
// below need. High-concurrency benchmarks consume many FDs per
// successful dial (client socket + sender bridge + listener bridge +
// echo socket + relay control sockets), and an EMFILE-driven failure
// reflects the host's ulimit rather than relay behavior.
//
// The actual rlimit check is delegated to platform-specific files
// (fdlimit_unix.go / fdlimit_other.go) because RLIMIT_NOFILE is not
// available on Windows.
func skipIfFDLimitTooLow(b *testing.B, need int) {
	b.Helper()
	if cur, ok := getFDLimit(); ok && uint64(need) > cur { //nolint:gosec // need is bounded by callers
		b.Skipf("benchmark needs >= %d file descriptors; soft limit is %d. Raise with `ulimit -n` and retry.", need, cur)
	}
}
