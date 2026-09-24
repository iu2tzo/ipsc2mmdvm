package mmdvm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iu2tzo/ipsc2mmdvm/internal/config"
	"github.com/iu2tzo/ipsc2mmdvm/internal/ipsc"
	"github.com/iu2tzo/ipsc2mmdvm/internal/metrics"
	"github.com/iu2tzo/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/iu2tzo/ipsc2mmdvm/internal/mmdvm/rewrite"
	"github.com/iu2tzo/ipsc2mmdvm/internal/timeslot"
)

type MMDVMClient struct {
	cfg     *config.MMDVM
	metrics *metrics.Metrics
	// verbose enables extra connection-flow logging (dial attempts,
	// reconnects, redundant Start()/Stop() calls, ...) at Info level.
	// Set from config.Config.LogsConnectionFlow() (true for log-level
	// "verbose" or "debug") by the caller of NewMMDVMClient.
	verbose bool
	started atomic.Bool
	// done is closed by Stop() to terminate the current run's goroutines.
	// It is replaced on every Start(), so code that may run concurrently
	// with Start() (e.g. HandleIPSCBurst) must read it via doneChan().
	done     chan struct{}
	runMu    sync.RWMutex // protects done
	stopOnce sync.Once
	wg       sync.WaitGroup
	// lifecycleMu serializes Start()/Stop() so the client can be safely
	// started and stopped repeatedly (e.g. driven by repeater
	// connect/disconnect events when config.IPSC.RequireRepeater is
	// enabled), rather than assuming a single start/stop over its
	// lifetime.
	lifecycleMu  sync.Mutex
	tx_chan      chan proto.Packet
	conn         net.Conn
	connMu       sync.Mutex // protects conn
	state        atomic.Uint32
	connRX       chan []byte
	connTX       chan []byte
	keepAlive    time.Duration
	timeout      time.Duration
	lastPing     atomic.Int64 // UnixNano — last MSTPONG received
	lastPingSent atomic.Int64 // UnixNano — last RPTPING sent
	// handshakeStarted (UnixNano) is when the current dial/login attempt
	// began; supervise() retries it if STATE_READY isn't reached within
	// timeout.
	handshakeStarted atomic.Int64
	ipscHandler      func(data []byte)
	translator       *ipsc.IPSCTranslator

	// Rewrite rules built from config, applied to packets
	// flowing through this network.
	rfRewrites      []rewrite.Rule // RF→Net (outbound to this master)
	netRewrites     []rewrite.Rule // Net→RF (inbound from this master)
	passallRewrites []rewrite.Rule // PassAll fallback for RF→Net

	// Timeslot managers prevent interleaved calls on the same slot.
	// outboundTSMgr is shared across all clients for the MMDVM→IPSC
	// direction. inboundTSMgr is per-client for the IPSC→MMDVM direction.
	outboundTSMgr *timeslot.Manager
	inboundTSMgr  *timeslot.Manager
}

type state uint8

const (
	STATE_IDLE state = iota
	STATE_SENT_LOGIN
	STATE_SENT_AUTH
	STATE_SENT_RPTC
	STATE_READY
	STATE_TIMEOUT
)

const (
	packetTypeMstack = "MSTACK"
)

// dialTimeout bounds a single dial attempt (mostly DNS resolution of the
// master's hostname).
const dialTimeout = 10 * time.Second

// retryDelay is how long the client waits before answering a login/auth/
// config rejection from the master with a new attempt.
const retryDelay = time.Second

// DMR frame type and data type constants for call termination detection.
const (
	frameTypeDataSync     uint = 2 // FrameType value for data sync (header/terminator)
	dtypeTerminatorWithLC uint = 2 // DataType value for Terminator with Link Control
)

func NewMMDVMClient(cfg *config.MMDVM, m *metrics.Metrics, verbose bool) *MMDVMClient {
	tx_chan := make(chan proto.Packet, 256)
	translator, err := ipsc.NewIPSCTranslator()
	if err != nil {
		slog.Warn("failed to load IPSC translator", "error", err)
	}
	c := &MMDVMClient{
		cfg:          cfg,
		metrics:      m,
		verbose:      verbose,
		done:         make(chan struct{}),
		tx_chan:      tx_chan,
		connRX:       make(chan []byte, 16),
		connTX:       make(chan []byte, 16),
		keepAlive:    5 * time.Second,
		timeout:      15 * time.Second,
		translator:   translator,
		inboundTSMgr: timeslot.NewManager(),
	}
	c.state.Store(uint32(STATE_IDLE))
	c.buildRewriteRules()
	if m != nil {
		if translator != nil {
			translator.SetMetrics(m)
		}
		c.inboundTSMgr.SetMetrics(m, "inbound")
	}
	return c
}

// Name returns the configured network name for this client.
func (h *MMDVMClient) Name() string {
	return h.cfg.Name
}

// buildRewriteRules constructs the rewrite rule chains from config.
// For each TGRewrite config entry, two rules are created:
//   - rfRewrite: fromSlot/fromTG → toSlot/toTG (for RF→Net direction)
//   - netRewrite: toSlot/toTG → fromSlot/fromTG (reverse, for Net→RF direction)
//
// PCRewrite only creates an RF rewrite (outbound).
// TypeRewrite only creates an RF rewrite (outbound).
// SrcRewrite only creates a Net rewrite (inbound).
func (h *MMDVMClient) buildRewriteRules() {
	name := h.cfg.Name

	for _, cfg := range h.cfg.TGRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.TGRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromTG: cfg.FromTG,
			ToSlot: cfg.ToSlot, ToTG: cfg.ToTG, Range: rng,
		})
		// Reverse direction
		h.netRewrites = append(h.netRewrites, &rewrite.TGRewrite{
			Name: name, FromSlot: cfg.ToSlot, FromTG: cfg.ToTG,
			ToSlot: cfg.FromSlot, ToTG: cfg.FromTG, Range: rng,
		})
	}

	for _, cfg := range h.cfg.PCRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.PCRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromID: cfg.FromID,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.cfg.TypeRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.rfRewrites = append(h.rfRewrites, &rewrite.TypeRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromTG: cfg.FromTG,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, cfg := range h.cfg.SrcRewrites {
		rng := cfg.Range
		if rng == 0 {
			rng = 1
		}
		h.netRewrites = append(h.netRewrites, &rewrite.SrcRewrite{
			Name: name, FromSlot: cfg.FromSlot, FromID: cfg.FromID,
			ToSlot: cfg.ToSlot, ToID: cfg.ToID, Range: rng,
		})
	}

	for _, slot := range h.cfg.PassAllTG {
		if slot < 0 {
			continue
		}
		s := uint(slot) //nolint:gosec
		r := &rewrite.PassAllTG{Name: name, Slot: s}
		h.passallRewrites = append(h.passallRewrites, r)
		h.netRewrites = append(h.netRewrites, &rewrite.PassAllTG{Name: name, Slot: s})
	}
	for _, slot := range h.cfg.PassAllPC {
		if slot < 0 {
			continue
		}
		s := uint(slot) //nolint:gosec
		r := &rewrite.PassAllPC{Name: name, Slot: s}
		h.passallRewrites = append(h.passallRewrites, r)
		h.netRewrites = append(h.netRewrites, &rewrite.PassAllPC{Name: name, Slot: s})
	}
}

// Start launches the client's goroutines and returns immediately; the
// connection to the master is established (and re-established) in the
// background by supervise(). It currently always returns nil: an
// unreachable master is retried rather than reported as an error.
func (h *MMDVMClient) Start() error {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	if h.started.Load() {
		// Already running; Start() is idempotent so callers (e.g. the
		// repeater-connected handler) don't need to track whether a
		// previous Start() already succeeded.
		h.verboseLog("MMDVM Start() called but client is already running", "network", h.cfg.Name)
		return nil
	}

	if h.translator != nil {
		h.translator.SetPeerID(h.cfg.ID)
	}

	slog.Info("Connecting to MMDVM server", "network", h.cfg.Name)

	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}

	// Re-create the per-run lifecycle primitives: done/stopOnce are
	// consumed by the previous Stop() call (if any) and can't be reused
	// across a Start/Stop cycle.
	h.runMu.Lock()
	h.done = make(chan struct{})
	h.runMu.Unlock()
	h.stopOnce = sync.Once{}

	// Don't replay packets left queued by a previous run.
	h.drainQueues()
	h.lastPing.Store(0)
	h.state.Store(uint32(STATE_IDLE))

	h.started.Store(true)

	// The dial + login is done by supervise(), which keeps retrying it
	// until the master answers: Start() itself never blocks on the
	// network (it may be called from the IPSC packet loop) and a master
	// that is unreachable right now (DNS not ready yet, link down, ...)
	// is simply retried instead of failing the start for good.
	h.wg.Add(5)
	go h.handler()
	go h.rx()
	go h.tx()
	go h.forwardTX()
	go h.supervise()

	return nil
}

// drainQueues discards anything left in the internal channels.
func (h *MMDVMClient) drainQueues() {
	for {
		select {
		case <-h.connTX:
		case <-h.connRX:
		case <-h.tx_chan:
		default:
			return
		}
	}
}

// doneChan returns the done channel of the current run.
func (h *MMDVMClient) doneChan() chan struct{} {
	h.runMu.RLock()
	defer h.runMu.RUnlock()
	return h.done
}

// pause waits for d, returning false early if the client is stopped.
func (h *MMDVMClient) pause(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-h.doneChan():
		return false
	}
}

// enqueue queues data for tx(). It never blocks past Stop(): tx() is gone
// by then and a plain send on a full connTX would hang the caller (and
// Stop()'s wg.Wait()) forever.
func (h *MMDVMClient) enqueue(data []byte) {
	select {
	case h.connTX <- data:
	case <-h.doneChan():
	}
}

func (h *MMDVMClient) connect() error {
	h.verboseLog("MMDVM dialing master server", "network", h.cfg.Name, "address", h.cfg.MasterServer)

	// Bound the dial (DNS) and abort it as soon as the client is stopped,
	// so Stop() never has to wait for a slow resolver.
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	done := h.doneChan()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", h.cfg.MasterServer)
	if err != nil {
		h.verboseLog("MMDVM dial failed", "network", h.cfg.Name, "address", h.cfg.MasterServer, "error", err)
		return err
	}
	h.connMu.Lock()
	h.conn = conn
	h.connMu.Unlock()

	h.verboseLog("MMDVM UDP connection established", "network", h.cfg.Name, "address", h.cfg.MasterServer)
	return nil
}

// verboseLog emits an Info-level log line only when verbose is enabled
// (log-level "verbose" or "debug", see config.Config.LogsConnectionFlow).
// Used for connection-flow events that are useful when debugging
// connectivity but too chatty to always show at log-level "info".
func (h *MMDVMClient) verboseLog(msg string, args ...any) {
	if h.verbose {
		slog.Info(msg, args...)
	}
}

const rptAck = "RPTACK"

func (h *MMDVMClient) handler() {
	defer h.wg.Done()
	done := h.doneChan()
	for {
		select {
		case data := <-h.connRX:
			slog.Debug("received packet", "data", fmt.Sprintf("% X", data), "strdata", string(data), "network", h.cfg.Name)
			if len(data) < 4 {
				slog.Warn("Ignoring short packet from MMDVM server", "network", h.cfg.Name, "length", len(data))
				continue
			}
			h.handleState(data)
		case <-done:
			return
		}
	}
}

func (h *MMDVMClient) handleState(data []byte) {
	currentState := h.state.Load()
	switch currentState {
	case uint32(STATE_IDLE):
		slog.Info("Got data from MMDVM server while idle", "network", h.cfg.Name)
	case uint32(STATE_SENT_LOGIN):
		h.handleSentLogin(data)
	case uint32(STATE_SENT_AUTH):
		h.handleSentAuth(data)
	case uint32(STATE_SENT_RPTC):
		h.handleSentRPTC(data)
	case uint32(STATE_READY):
		h.handleReady(data)
	case uint32(STATE_TIMEOUT):
		slog.Info("Got data from MMDVM server while in timeout state", "network", h.cfg.Name)
	}
}

func (h *MMDVMClient) handleSentLogin(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		if len(data) < 10 {
			slog.Warn("RPTACK response too short", "network", h.cfg.Name, "length", len(data))
			return
		}
		slog.Info("Connected. Authenticating", "network", h.cfg.Name)
		random := data[len(data)-4:]
		h.sendRPTK(random)
		h.state.Store(uint32(STATE_SENT_AUTH))
	} else {
		slog.Info("Server rejected login request", "network", h.cfg.Name)
		if !h.pause(retryDelay) {
			return
		}
		h.sendLogin()
	}
}

func (h *MMDVMClient) handleSentAuth(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Authenticated. Sending configuration", "network", h.cfg.Name)
		h.state.Store(uint32(STATE_SENT_RPTC))
		h.sendRPTC()
	} else if len(data) >= 6 && string(data[:6]) == "RPTNAK" {
		slog.Info("Password rejected", "network", h.cfg.Name)
		if h.metrics != nil {
			h.metrics.MMDVMAuthFailures.WithLabelValues(h.cfg.Name).Inc()
		}
		h.state.Store(uint32(STATE_SENT_LOGIN))
		if !h.pause(retryDelay) {
			return
		}
		h.sendLogin()
	}
}

func (h *MMDVMClient) handleSentRPTC(data []byte) {
	if len(data) >= 6 && string(data[:6]) == rptAck {
		slog.Info("Config accepted, starting ping routine", "network", h.cfg.Name)
		// From here on supervise() keeps the connection alive with
		// periodic pings; send the first one right away.
		h.lastPing.Store(time.Now().UnixNano())
		h.state.Store(uint32(STATE_READY))
		if h.metrics != nil {
			h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(2)
		}
		h.sendPing()
	} else if len(data) >= 6 && string(data[:6]) == "MSTNAK" {
		slog.Info("Configuration rejected", "network", h.cfg.Name)
		if !h.pause(retryDelay) {
			return
		}
		h.sendRPTC()
	}
}

func (h *MMDVMClient) handleReady(data []byte) {
	switch string(data[:4]) {
	case "MSTP":
		if len(data) >= 7 && string(data[:7]) == "MSTPONG" {
			now := time.Now()
			if h.metrics != nil {
				sent := time.Unix(0, h.lastPingSent.Load())
				if !sent.IsZero() {
					h.metrics.MMDVMPingRTT.WithLabelValues(h.cfg.Name).Observe(now.Sub(sent).Seconds())
				}
			}
			h.lastPing.Store(now.UnixNano())
		}
	case "RPTS":
		if len(data) >= 7 && string(data[:7]) == "RPTSBKN" {
			slog.Info("Server requested a roaming beacon transmission", "network", h.cfg.Name)
		}
	case "DMRD":
		packet, ok := proto.Decode(data)
		if !ok {
			slog.Info("Error unpacking packet", "network", h.cfg.Name)
			return
		}
		if h.metrics != nil {
			h.metrics.MMDVMPacketsReceived.WithLabelValues(h.cfg.Name).Inc()
		}
		slog.Debug("MMDVM DMRD received", "network", h.cfg.Name, "packet", packet)

		if !rewrite.Apply(h.netRewrites, &packet) {
			slog.Debug("MMDVM DMRD dropped (no rewrite rule matched)", "network", h.cfg.Name)
			if h.metrics != nil {
				h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
			}
			return
		}

		slog.Debug("MMDVM DMRD after rewrite", "network", h.cfg.Name, "packet", packet)

		// Timeslot arbitration: buffer competing calls, deliver FIFO.
		isTerminator := packet.FrameType == frameTypeDataSync && packet.DTypeOrVSeq == dtypeTerminatorWithLC
		if h.outboundTSMgr != nil {
			if !h.outboundTSMgr.Submit(packet.Slot, packet.StreamID, h.cfg.Name, packet) {
				slog.Debug("MMDVM DMRD buffered (timeslot busy)",
					"network", h.cfg.Name, "slot", packet.Slot, "streamID", packet.StreamID)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "timeslot_busy").Inc()
				}
				return
			}
		}

		h.translateAndForwardToIPSC(packet)

		if isTerminator && h.outboundTSMgr != nil {
			h.drainPendingOutbound(packet.Slot, packet.StreamID)
		}
	default:
		slog.Info("Got unknown packet from MMDVM server", "network", h.cfg.Name, "data", data)
	}
}

// supervise owns the connection to the master for the whole run: it
// performs the initial dial + login, keeps the session alive with pings
// once STATE_READY is reached, and starts over (reconnect) whenever the
// master stops answering, whether that happens during the handshake
// (no answer within timeout, including a failed dial) or afterwards (no
// MSTPONG within timeout). Being the only goroutine that reconnects, and
// never exiting before Stop(), it guarantees the client can't get stuck
// in a half-open handshake state.
func (h *MMDVMClient) supervise() {
	defer h.wg.Done()
	done := h.doneChan()

	h.connMu.Lock()
	hasConn := h.conn != nil
	h.connMu.Unlock()
	if !hasConn {
		h.dialAndLogin()
	}

	ticker := time.NewTicker(h.keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.checkConnection()
		case <-done:
			return
		}
	}
}

// checkConnection runs one supervise() step: ping (or reconnect on
// timeout) when ready, otherwise retry a handshake that is taking too long.
func (h *MMDVMClient) checkConnection() {
	now := time.Now()
	st := state(h.state.Load() & 0xFF) //nolint:gosec

	if st == STATE_READY {
		lastPingTime := time.Unix(0, h.lastPing.Load())
		if now.After(lastPingTime.Add(h.timeout)) {
			slog.Info("Connection timed out", "network", h.cfg.Name)
			h.reconnect()
			return
		}
		h.sendPing()
		return
	}

	started := time.Unix(0, h.handshakeStarted.Load())
	if now.After(started.Add(h.timeout)) {
		slog.Warn("Handshake timed out, reconnecting", "network", h.cfg.Name, "state", st)
		h.reconnect()
	}
}

// reconnect closes the current connection, dials a new one, and
// sends a fresh login. Only called from supervise().
func (h *MMDVMClient) reconnect() {
	h.verboseLog("MMDVM reconnecting", "network", h.cfg.Name)
	h.state.Store(uint32(STATE_TIMEOUT))
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
		h.metrics.MMDVMReconnects.WithLabelValues(h.cfg.Name).Inc()
	}
	h.connMu.Lock()
	if h.conn != nil {
		if err := h.conn.Close(); err != nil {
			slog.Error("Error closing connection", "network", h.cfg.Name, "error", err)
		}
		h.conn = nil
	}
	h.connMu.Unlock()
	h.dialAndLogin()
}

// dialAndLogin dials the master and sends RPTL. On a dial failure the
// client stays in STATE_TIMEOUT with no connection, and supervise()
// tries again once timeout has elapsed.
func (h *MMDVMClient) dialAndLogin() {
	h.handshakeStarted.Store(time.Now().UnixNano())
	if err := h.connect(); err != nil {
		slog.Error("Error connecting to MMDVM server, will retry", "network", h.cfg.Name, "error", err, "retryIn", h.timeout)
		h.state.Store(uint32(STATE_TIMEOUT))
		return
	}
	if h.metrics != nil {
		h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(1)
	}
	h.state.Store(uint32(STATE_SENT_LOGIN))
	h.sendLogin()
}

func (h *MMDVMClient) tx() {
	defer h.wg.Done()
	done := h.doneChan()
	for {
		select {
		case <-done:
			return
		case data := <-h.connTX:
			h.connMu.Lock()
			slog.Debug("sending packet", "data", fmt.Sprintf("% X", data), "strdata", string(data), "network", h.cfg.Name)
			err := net.ErrClosed
			if h.conn != nil {
				_, err = h.conn.Write(data)
			}
			h.connMu.Unlock()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					// No usable connection (reconnect in progress or
					// failed): drop the packet instead of re-queueing
					// it, so connTX keeps draining and never fills up.
					// Stale voice is useless and supervise() restarts
					// the login from scratch on the new connection.
					slog.Debug("MMDVM connection not available, dropping packet", "network", h.cfg.Name)
					continue
				}
				slog.Error("Error writing to MMDVM server", "network", h.cfg.Name, "error", err)
			}
		}
	}
}

func (h *MMDVMClient) rx() {
	defer h.wg.Done()
	done := h.doneChan()
	buf := make([]byte, 512)
	for {
		h.connMu.Lock()
		conn := h.conn
		h.connMu.Unlock()
		if conn == nil {
			// Not connected (yet): wait for supervise() to dial.
			select {
			case <-time.After(100 * time.Millisecond):
				continue
			case <-done:
				return
			}
		}
		n, err := conn.Read(buf)
		if err != nil {
			if !h.started.Load() {
				return
			}
			// If the connection was closed (e.g. by a reconnect), loop
			// back and pick up the new h.conn instead of exiting the
			// goroutine.
			if errors.Is(err, net.ErrClosed) {
				select {
				case <-time.After(100 * time.Millisecond):
					continue
				case <-done:
					return
				}
			}
			slog.Error("Error reading from MMDVM server", "network", h.cfg.Name, "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case h.connRX <- data:
		case <-done:
			return
		}
	}
}

func (h *MMDVMClient) Stop() {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	if !h.started.Load() {
		// Not running (never started, or already stopped): nothing to
		// do. Keeps Stop() safe to call unconditionally, e.g. from the
		// repeater-disconnected handler or shutdown path regardless of
		// whether a repeater was ever seen.
		h.verboseLog("MMDVM Stop() called but client is not running", "network", h.cfg.Name)
		return
	}

	h.stopOnce.Do(func() {
		slog.Info("Stopping MMDVM client", "network", h.cfg.Name)

		// Signal all goroutines to stop.
		close(h.doneChan())

		// Send the disconnect message directly on the wire (best-effort).
		h.connMu.Lock()
		if h.conn != nil {
			h.sendRPTCLDirect()
			h.conn.Close()
			// So the next Start() dials a fresh connection.
			h.conn = nil
		}
		h.connMu.Unlock()

		h.started.Store(false)
		if h.metrics != nil {
			h.metrics.MMDVMConnectionState.WithLabelValues(h.cfg.Name).Set(0)
		}
	})

	// Wait for all goroutines to finish.
	h.wg.Wait()
}

// sendRPTCLDirect writes the disconnect message directly on the connection.
// Must be called with connMu held.
func (h *MMDVMClient) sendRPTCLDirect() {
	hexid := make([]byte, 8)
	copy(hexid, []byte(fmt.Sprintf("%08x", h.cfg.ID)))
	data := make([]byte, len("RPTCL")+8)
	n := copy(data, "RPTCL")
	copy(data[n:], hexid)
	if _, err := h.conn.Write(data); err != nil {
		slog.Error("Error sending RPTCL disconnect", "network", h.cfg.Name, "error", err)
	}
}

func (h *MMDVMClient) forwardTX() {
	defer h.wg.Done()
	done := h.doneChan()
	for {
		select {
		case <-done:
			return
		case pkt := <-h.tx_chan:
			h.sendPacket(pkt)
		}
	}
}

// translateAndForwardToIPSC converts a proto.Packet to IPSC and sends it.
func (h *MMDVMClient) translateAndForwardToIPSC(packet proto.Packet) {
	if h.ipscHandler != nil && h.translator != nil {
		ipscPackets := h.translator.TranslateToIPSC(packet)
		for _, ipscData := range ipscPackets {
			h.ipscHandler(ipscData)
		}
	}
}

// drainPendingOutbound delivers buffered pending calls on the given slot
// after the active stream terminates (MMDVM→IPSC direction). If a pending
// call's packets include a terminator, it chains to the next pending call.
func (h *MMDVMClient) drainPendingOutbound(slot bool, streamID uint) {
	currentStreamID := streamID
	for {
		buffered := h.outboundTSMgr.Release(slot, currentStreamID)
		if len(buffered) == 0 {
			return
		}
		var nextStreamID uint
		hasTerminator := false
		for _, item := range buffered {
			pkt, ok := item.(proto.Packet)
			if !ok {
				continue
			}
			h.translateAndForwardToIPSC(pkt)
			if pkt.FrameType == frameTypeDataSync && pkt.DTypeOrVSeq == dtypeTerminatorWithLC {
				hasTerminator = true
				nextStreamID = pkt.StreamID
			}
		}
		if !hasTerminator {
			return
		}
		currentStreamID = nextStreamID
	}
}

// drainPendingInbound delivers buffered pending calls on the given slot
// after the active stream terminates (IPSC→MMDVM direction). Returns
// false if the done channel was signaled.
func (h *MMDVMClient) drainPendingInbound(slot bool, streamID uint) bool {
	done := h.doneChan()
	currentStreamID := streamID
	for {
		buffered := h.inboundTSMgr.Release(slot, currentStreamID)
		if len(buffered) == 0 {
			return true
		}
		var nextStreamID uint
		hasTerminator := false
		for _, item := range buffered {
			pkt, ok := item.(proto.Packet)
			if !ok {
				continue
			}
			select {
			case h.tx_chan <- pkt:
			case <-done:
				return false
			}
			if pkt.FrameType == frameTypeDataSync && pkt.DTypeOrVSeq == dtypeTerminatorWithLC {
				hasTerminator = true
				nextStreamID = pkt.StreamID
			}
		}
		if !hasTerminator {
			return true
		}
		currentStreamID = nextStreamID
	}
}

func (h *MMDVMClient) SetIPSCHandler(handler func(data []byte)) {
	h.ipscHandler = handler
}

// SetOutboundTSManager sets the shared timeslot manager used for the
// MMDVM→IPSC direction. This manager is shared across all clients so
// that only one MMDVM master can feed a given timeslot at a time.
func (h *MMDVMClient) SetOutboundTSManager(mgr *timeslot.Manager) {
	h.outboundTSMgr = mgr
}

// MatchesRules checks whether the given IPSC data would match this client's
// rewrite rules without translating or modifying any state. It extracts
// routing-relevant fields (src, dst, groupCall, slot) directly from the
// IPSC packet header. When passallOnly is true, only passall rules are checked.
func (h *MMDVMClient) MatchesRules(packetType byte, data []byte, passallOnly bool) bool {
	if len(data) < 18 {
		return false
	}
	// Build a probe packet from IPSC header fields.
	probe := proto.Packet{
		Src:       uint(data[6])<<16 | uint(data[7])<<8 | uint(data[8]),
		Dst:       uint(data[9])<<16 | uint(data[10])<<8 | uint(data[11]),
		GroupCall: packetType == 0x80 || packetType == 0x83,
		Slot:      (data[17] & 0x20) != 0,
	}
	if passallOnly {
		return rewrite.Apply(h.passallRewrites, &probe)
	}
	return rewrite.Apply(h.rfRewrites, &probe)
}

// HandleIPSCBurst handles an incoming IPSC burst from the IPSC server.
// This is called when a connected IPSC peer transmits voice/data.
// It translates the IPSC packet(s) to MMDVM DMRD format and forwards them.
// Callers should use MatchesRules first to determine which network wins,
// then call HandleIPSCBurst only on the winning client.
func (h *MMDVMClient) HandleIPSCBurst(packetType byte, data []byte, addr *net.UDPAddr) bool {
	if !h.started.Load() {
		return false
	}
	done := h.doneChan()
	slog.Debug("HandleIPSCBurst: received IPSC burst", "network", h.cfg.Name, "type", packetType, "from", addr, "length", len(data))

	packets := h.translator.TranslateToMMDVM(packetType, data)
	matched := false
	for _, pkt := range packets {
		slog.Debug("HandleIPSCBurst: pre-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)
		// Apply RF→Net rewrite rules (outbound to this master).
		// Try specific rewrites first; if none match, try passall
		// rules as a fallback.
		if !rewrite.Apply(h.rfRewrites, &pkt) {
			if !rewrite.Apply(h.passallRewrites, &pkt) {
				slog.Debug("HandleIPSCBurst: dropped (no rewrite rule matched)", "network", h.cfg.Name)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "no_rewrite").Inc()
				}
				continue
			}
		}
		slog.Debug("HandleIPSCBurst: post-rewrite", "network", h.cfg.Name, "src", pkt.Src, "dst", pkt.Dst, "groupCall", pkt.GroupCall, "slot", pkt.Slot)

		// Timeslot arbitration: buffer competing calls, deliver FIFO.
		isTerminator := pkt.FrameType == frameTypeDataSync && pkt.DTypeOrVSeq == dtypeTerminatorWithLC
		if h.inboundTSMgr != nil {
			if !h.inboundTSMgr.Submit(pkt.Slot, pkt.StreamID, "ipsc", pkt) {
				slog.Debug("HandleIPSCBurst: buffered (timeslot busy)",
					"network", h.cfg.Name, "slot", pkt.Slot, "streamID", pkt.StreamID)
				if h.metrics != nil {
					h.metrics.MMDVMPacketsDropped.WithLabelValues(h.cfg.Name, "timeslot_busy").Inc()
				}
				continue
			}
		}

		matched = true

		select {
		case h.tx_chan <- pkt:
		case <-done:
			return matched
		}

		if isTerminator && h.inboundTSMgr != nil {
			if !h.drainPendingInbound(pkt.Slot, pkt.StreamID) {
				return matched
			}
		}
	}
	return matched
}
