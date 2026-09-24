package ipsc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iu2tzo/ipsc2mmdvm/internal/config"
	"github.com/iu2tzo/ipsc2mmdvm/internal/metrics"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type IPSCServer struct {
	cfg     *config.Config
	metrics *metrics.Metrics
	udp     *net.UDPConn
	mu      sync.RWMutex

	localID  uint32
	authKey  []byte // 20-byte HMAC key decoded from hex
	peers    map[uint32]*Peer
	lastSend map[uint32]time.Time

	// repeaterConnected tracks whether at least one peer is currently
	// registered, so peerStateHandler is only invoked on 0<->1+ peer
	// transitions rather than on every packet.
	repeaterConnected bool
	// repeaterTimeout is how long a peer may go without traffic before
	// it is considered disconnected and reaped.
	repeaterTimeout time.Duration
	peerStateHandler func(connected bool)

	burstHandler func(packetType byte, data []byte, addr *net.UDPAddr)

	done     chan struct{}
	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopOnce sync.Once
}

type Packet struct {
	data []byte
}

type Peer struct {
	ID                 uint32
	Addr               *net.UDPAddr
	Mode               byte
	Flags              [4]byte
	LastSeen           time.Time
	KeepAliveReceived  uint64
	RegistrationStatus bool
}

type PacketType byte

const (
	PacketType_GroupVoice            PacketType = 0x80
	PacketType_PrivateVoice          PacketType = 0x81
	PacketType_GroupData             PacketType = 0x83
	PacketType_PrivateData           PacketType = 0x84
	PacketType_RepeaterWakeUp        PacketType = 0x85
	PacketType_MasterRegisterRequest PacketType = 0x90
	PacketType_MasterRegisterReply   PacketType = 0x91
	PacketType_PeerListRequest       PacketType = 0x92
	PacketType_PeerListReply         PacketType = 0x93
	PacketType_MasterAliveRequest    PacketType = 0x96
	PacketType_MasterAliveReply      PacketType = 0x97
)

var (
	//nolint:gochecknoglobals
	ipscVersion = []byte{0x04, 0x02, 0x04, 0x01}
)

var ErrPacketIgnored = errors.New("packet ignored")

func NewIPSCServer(cfg *config.Config, m *metrics.Metrics) *IPSCServer {
	// Decode the auth key from hex string to raw bytes.
	// DMRlink left-pads the hex key to 40 characters (20 bytes) with zeros.
	var authKey []byte
	if cfg.IPSC.Auth.Enabled && cfg.IPSC.Auth.Key != "" {
		hexKey := cfg.IPSC.Auth.Key
		// Left-pad with zeros to 40 hex characters (20 bytes)
		for len(hexKey) < 40 {
			hexKey = "0" + hexKey
		}
		var err error
		authKey, err = hex.DecodeString(hexKey)
		if err != nil {
			slog.Error("failed to decode IPSC auth key as hex, using raw string", "error", err)
			authKey = []byte(cfg.IPSC.Auth.Key)
		}
	}

	// Use the first MMDVM network's ID as the local peer identity.
	var localID uint32
	if len(cfg.MMDVM) > 0 {
		localID = cfg.MMDVM[0].ID
	}

	// Default the repeater timeout if it's unset, so a zero value never
	// results in peers being reaped immediately (Validate() already
	// rejects this combination, but guard against a zero value reaching
	// here from tests/direct construction too).
	repeaterTimeout := time.Duration(cfg.IPSC.RepeaterTimeout) * time.Second
	if repeaterTimeout <= 0 {
		repeaterTimeout = time.Duration(config.DefaultRepeaterTimeoutSeconds) * time.Second
	}

	return &IPSCServer{
		cfg:             cfg,
		metrics:         m,
		localID:         localID,
		authKey:         authKey,
		peers:           map[uint32]*Peer{},
		lastSend:        map[uint32]time.Time{},
		repeaterTimeout: repeaterTimeout,
		done:            make(chan struct{}),
	}
}

// SetPeerConnectionHandler registers a callback invoked whenever the
// registered-peer count transitions to/from zero, i.e. when the first
// repeater peer registers ("connected") or the last known repeater peer
// is reaped for inactivity ("disconnected"). Used by callers that only
// want to open the DMR network connections while a repeater is present
// (config.IPSC.RequireRepeater).
func (s *IPSCServer) SetPeerConnectionHandler(handler func(connected bool)) {
	s.peerStateHandler = handler
}

// startPeerReaper periodically removes peers that haven't sent any
// traffic within repeaterTimeout, and reports repeater connect/disconnect
// transitions via peerStateHandler (if one is set).
func (s *IPSCServer) startPeerReaper() {
	interval := s.repeaterTimeout / 3
	if interval < time.Second {
		interval = time.Second
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reapStalePeers()
			case <-s.done:
				return
			}
		}
	}()
}

// reapStalePeers removes peers that have not been heard from within
// repeaterTimeout and, if the registered-peer count transitions to/from
// zero as a result, notifies peerStateHandler.
func (s *IPSCServer) reapStalePeers() {
	now := time.Now()

	s.mu.Lock()
	var removed []uint32
	for id, peer := range s.peers {
		if now.Sub(peer.LastSeen) > s.repeaterTimeout {
			delete(s.peers, id)
			delete(s.lastSend, id)
			removed = append(removed, id)
		}
	}
	if s.metrics != nil {
		s.metrics.IPSCPeersRegistered.Set(float64(len(s.peers)))
	}
	nowConnected, wasConnected, handler := s.noteConnectionStateLocked()
	s.mu.Unlock()

	for _, id := range removed {
		s.verboseLog("IPSC peer timed out, removed", "peerID", id, "timeout", s.repeaterTimeout)
	}
	s.notifyConnectionState(nowConnected, wasConnected, handler)
}

// Start opens the IPSC listener according to the mode derived from
// cfg.IPSC (see config.IPSC.Mode). Callers are expected to have already
// run config.Config.Validate() beforehand.
func (s *IPSCServer) Start() error {
	var err error
	switch s.cfg.IPSC.Mode() {
	case config.IPSCModeManaged:
		if err = s.netlink(); err != nil {
			return fmt.Errorf("error configuring network: %w", err)
		}
		err = s.listenOnIP(s.cfg.IPSC.IP, s.cfg.IPSC.Port)

	case config.IPSCModeBindDevice:
		err = s.listenOnInterface(s.cfg.IPSC.Interface, s.cfg.IPSC.Port)

	case config.IPSCModeAny:
		err = s.listenOnIP("0.0.0.0", s.cfg.IPSC.Port)

	default: // config.IPSCModeInvalid
		// Shouldn't happen if Validate() was called before Start(),
		// but keep it as a safety net.
		return fmt.Errorf("invalid IPSC configuration: interface=%q ip=%q",
			s.cfg.IPSC.Interface, s.cfg.IPSC.IP)
	}
	if err != nil {
		return err
	}

	s.verboseLog("IPSC server listening", "mode", s.cfg.IPSC.Mode(), "interface", s.cfg.IPSC.Interface, "ip", s.cfg.IPSC.IP, "port", s.cfg.IPSC.Port)

	// Always reap stale peers, so a repeater that went silent stops being
	// sent traffic and listed in peer-list replies. Whether its
	// disappearance also tears down the DMR network connections is up to
	// the peerStateHandler (only installed with RequireRepeater).
	s.verboseLog("IPSC repeater timeout tracking enabled", "repeaterTimeout", s.repeaterTimeout)
	s.startPeerReaper()

	return nil
}

// verboseLog emits an Info-level log line only when log-level is "verbose"
// or "debug" (see config.Config.LogsConnectionFlow). Used throughout this
// file for connection-flow events (listener start, peer register/timeout,
// ...) that are useful when debugging connectivity but too chatty to
// always show at log-level "info".
func (s *IPSCServer) verboseLog(msg string, args ...any) {
	if s.cfg.LogsConnectionFlow() {
		slog.Info(msg, args...)
	}
}

// listenOnIP opens a plain UDP listener bound to the given address, with
// no interface/address management (config.IPSCModeManaged and
// config.IPSCModeAny both end up here, the former after netlink() has
// already assigned ip to the interface).
func (s *IPSCServer) listenOnIP(ip string, port uint16) error {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: int(port),
	})
	if err != nil {
		return fmt.Errorf("error starting UDP listener on %s:%d: %w", ip, port, err)
	}

	s.udp = udp
	s.wg.Add(1)
	go s.handler()
	return nil
}

// listenOnInterface opens a UDP listener bound to a specific network
// interface (SO_BINDTODEVICE) rather than to a specific IP address. The
// interface's own address(es) are left untouched: whatever is already
// configured on it (static, DHCP, assigned by wg-quick, ...) is used
// as-is. The socket accepts traffic on any address currently configured
// on that interface, on the given port.
//
// Requires CAP_NET_RAW (in practice: root), same as netlink() below.
// Linux-specific, consistent with the rest of this package.
func (s *IPSCServer) listenOnInterface(ifaceName string, port uint16) error {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			ctrlErr := c.Control(func(fd uintptr) {
				sockErr = unix.SetsockoptString(
					int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifaceName,
				)
			})
			if ctrlErr != nil {
				return ctrlErr
			}
			return sockErr
		},
	}

	pc, err := lc.ListenPacket(context.Background(), "udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("error binding UDP listener to interface %q: %w", ifaceName, err)
	}

	udp, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return fmt.Errorf("unexpected connection type binding to interface %q", ifaceName)
	}

	s.udp = udp
	s.wg.Add(1)
	go s.handler()
	return nil
}

func (s *IPSCServer) Stop() {
	s.stopOnce.Do(func() {
		slog.Info("Stopping IPSC server")
		s.stopped.Store(true)
		close(s.done)
		if s.udp != nil {
			if err := s.udp.Close(); err != nil {
				slog.Error("error closing UDP listener", "error", err)
			}
		}
	})
	s.wg.Wait()
}

// netlink fully manages the addressing of cfg.IPSC.Interface: existing
// addresses are removed and cfg.IPSC.IP/SubnetMask is assigned. Only
// used in config.IPSCModeManaged.
func (s *IPSCServer) netlink() error {
	link, err := netlink.LinkByName(s.cfg.IPSC.Interface)
	if err != nil {
		return fmt.Errorf("cannot find interface %s: %w", s.cfg.IPSC.Interface, err)
	}

	// Remove any existing addresses from the interface
	existingAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("cannot list addresses on interface %s: %w", s.cfg.IPSC.Interface, err)
	}
	for i := range existingAddrs {
		if err := netlink.AddrDel(link, &existingAddrs[i]); err != nil {
			return fmt.Errorf("cannot remove address %s from interface %s: %w", existingAddrs[i].IPNet, s.cfg.IPSC.Interface, err)
		}
	}

	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(s.cfg.IPSC.IP), Mask: net.CIDRMask(s.cfg.IPSC.SubnetMask, 32)}}); err != nil {
		return fmt.Errorf("cannot add IP address to interface %s: %w", s.cfg.IPSC.Interface, err)
	}

	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("cannot set interface up %s: %w", s.cfg.IPSC.Interface, err)
		}
	}

	return nil
}

func (s *IPSCServer) handler() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if s.metrics != nil {
				s.metrics.IPSCUDPErrors.WithLabelValues("read").Inc()
			}
			slog.Warn("error reading from UDP", "error", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		s.wg.Add(1)
		go func(packetData []byte, packetAddr *net.UDPAddr) {
			defer s.wg.Done()
			packet, err := s.handlePacket(packetData, packetAddr)
			if err != nil {
				if errors.Is(err, ErrPacketIgnored) {
					return
				}
				slog.Warn("error parsing packet", "peer", packetAddr, "error", err, "length", len(packetData), "packet", packetData)
				return
			}

			slog.Debug("received packet", "peer", packetAddr, "length", len(packetData), "packet", packet)
		}(data, addr)
	}
}

func (s *IPSCServer) handlePacket(data []byte, addr *net.UDPAddr) (*Packet, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("packet too short")
	}

	packetType := data[0]
	// The IPSC protocol has no separate login/password packet: instead,
	// every packet is signed with an HMAC-SHA1 built from the shared
	// auth key (IPSC.Auth.Key), appended as its last 10 bytes. For the
	// connection-request packet (MASTER_REGISTER_REQUEST) this signature
	// is effectively the repeater "sending its connection password", so
	// it's logged distinctly (at verbose level) from the ongoing
	// per-packet authentication of ordinary traffic (logged at debug
	// level, since it happens on every single packet).
	isConnectionRequest := PacketType(packetType) == PacketType_MasterRegisterRequest

	if s.cfg.IPSC.Auth.Enabled {
		if len(data) <= 10 {
			return nil, fmt.Errorf("packet too short for authentication")
		}

		if isConnectionRequest {
			s.verboseLog("IPSC repeater sent connection credentials", "peer", addr)
		}

		if !s.auth(data) {
			if s.metrics != nil {
				s.metrics.IPSCAuthFailures.Inc()
			}
			if isConnectionRequest {
				s.verboseLog("IPSC repeater authentication failed", "peer", addr)
			} else {
				slog.Debug("IPSC packet authentication failed", "peer", addr, "packetType", packetType)
			}
			return nil, fmt.Errorf("authentication failed")
		}

		if isConnectionRequest {
			s.verboseLog("IPSC repeater authentication succeeded", "peer", addr)
		} else {
			slog.Debug("IPSC packet authentication succeeded", "peer", addr, "packetType", packetType)
		}

		data = data[:len(data)-10] // Remove the hash from the data
	}

	switch PacketType(packetType) {
	case PacketType_GroupVoice:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("group_voice").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateVoice:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("private_voice").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_GroupData:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("group_data").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_PrivateData:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("private_data").Inc()
		}
		if err := s.handleUserPacket(PacketType(packetType), data, addr); err != nil {
			return nil, err
		}
	case PacketType_RepeaterWakeUp:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("wake_up").Inc()
		}
		if err := s.handleRepeaterWakeUp(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("register").Inc()
		}
		if err := s.handleMasterRegisterRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterAliveRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("alive").Inc()
		}
		if err := s.handleMasterAliveRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_PeerListRequest:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("peer_list").Inc()
		}
		if err := s.handlePeerListRequest(data, addr); err != nil {
			return nil, err
		}
	case PacketType_MasterRegisterReply, PacketType_PeerListReply, PacketType_MasterAliveReply:
		// These are reply packets, we shouldn't receive them as a server, keeping quiet.
		return nil, ErrPacketIgnored
	default:
		if s.metrics != nil {
			s.metrics.IPSCPacketsReceived.WithLabelValues("other").Inc()
		}
		return nil, fmt.Errorf("unknown packet type: %d", packetType)
	}

	return &Packet{data: data}, nil
}

func (s *IPSCServer) handleMasterRegisterRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	// This is the repeater opening (or renewing) its connection to us:
	// the first packet of the IPSC "master registration" exchange.
	s.verboseLog("IPSC connection request received from repeater", "peerID", peerID, "addr", addr)

	mode := s.defaultModeByte()
	flags := s.defaultFlagsBytes()
	if len(data) >= 10 {
		mode = data[5]
		copy(flags[:], data[6:10])
	}

	s.upsertPeer(peerID, addr, mode, flags)

	packet := &Packet{data: s.buildMasterRegisterReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master register reply: %w", err)
	}

	// Reply sent successfully: the connection with this repeater is now
	// established (it will keep it alive with MASTER_ALIVE/keepalive
	// packets from here on).
	s.verboseLog("IPSC connection established with repeater", "peerID", peerID, "addr", addr)

	return nil
}

func (s *IPSCServer) handleMasterAliveRequest(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	// Like DMRlink, only answer keep-alives from peers that completed the
	// MASTER_REGISTER exchange. Staying silent makes an unknown repeater
	// (e.g. reaped for inactivity, or after a restart of this server) time
	// out on its side and register again, instead of being kept alive as
	// a half-known peer with no mode/flags.
	if !s.refreshRegisteredPeer(peerID, addr, data) {
		slog.Warn("IPSC keep-alive request from unregistered peer ignored, waiting for it to register", "peerID", peerID, "addr", addr)
		return ErrPacketIgnored
	}
	s.verboseLog("IPSC keep-alive request received from repeater", "peerID", peerID, "addr", addr)

	packet := &Packet{data: s.buildMasterAliveReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending master alive reply: %w", err)
	}
	s.verboseLog("IPSC keep-alive reply sent to repeater", "peerID", peerID, "addr", addr)

	return nil
}

// refreshRegisteredPeer handles a MASTER_ALIVE_REQUEST for peerID: if the
// peer is registered, its address, LastSeen and keep-alive counter are
// updated, as are its mode/flags when the packet carries them (same
// layout as MASTER_REGISTER_REQUEST). Returns false, leaving the peer
// table untouched, if the peer is unknown or not registered.
func (s *IPSCServer) refreshRegisteredPeer(peerID uint32, addr *net.UDPAddr, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, ok := s.peers[peerID]
	if !ok || !peer.RegistrationStatus {
		return false
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.LastSeen = time.Now()
	peer.KeepAliveReceived++
	if len(data) >= 10 {
		peer.Mode = data[5]
		copy(peer.Flags[:], data[6:10])
	}
	return true
}

func (s *IPSCServer) handlePeerListRequest(data []byte, addr *net.UDPAddr) error {
	if _, err := parsePeerID(data); err != nil {
		return err
	}

	packet := &Packet{data: s.buildPeerListReply()}
	if err := s.sendPacket(packet, addr); err != nil {
		return fmt.Errorf("error sending peer list reply: %w", err)
	}

	return nil
}

func (s *IPSCServer) handleRepeaterWakeUp(data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("repeater wake-up packet received", "peer", addr, "peerID", peerID, "length", len(data))
	return nil
}

func (s *IPSCServer) handleUserPacket(packetType PacketType, data []byte, addr *net.UDPAddr) error {
	peerID, err := parsePeerID(data)
	if err != nil {
		return err
	}

	s.markPeerAlive(peerID, addr)
	slog.Debug("IPSC burst received", "peer", addr, "peerID", peerID, "packetType", byte(packetType), "length", len(data))
	if s.burstHandler != nil {
		packetCopy := make([]byte, len(data))
		copy(packetCopy, data)
		go s.burstHandler(byte(packetType), packetCopy, addr)
	}
	return nil
}

func (s *IPSCServer) SetBurstHandler(handler func(packetType byte, data []byte, addr *net.UDPAddr)) {
	s.burstHandler = handler
}

func (s *IPSCServer) upsertPeer(peerID uint32, addr *net.UDPAddr, mode byte, flags [4]byte) {
	s.mu.Lock()

	peer, existed := s.peers[peerID]
	if !existed {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.Mode = mode
	peer.Flags = flags
	peer.LastSeen = time.Now()
	peer.RegistrationStatus = true

	if s.metrics != nil {
		s.metrics.IPSCPeersRegistered.Set(float64(len(s.peers)))
	}

	nowConnected, wasConnected, handler := s.noteConnectionStateLocked()
	s.mu.Unlock()

	if !existed {
		s.verboseLog("IPSC peer registered", "peerID", peerID, "addr", addr)
	} else {
		s.verboseLog("IPSC peer re-registered", "peerID", peerID, "addr", addr)
	}
	s.notifyConnectionState(nowConnected, wasConnected, handler)
}

func (s *IPSCServer) markPeerAlive(peerID uint32, addr *net.UDPAddr) {
	s.mu.Lock()

	peer, ok := s.peers[peerID]
	if !ok {
		peer = &Peer{ID: peerID}
		s.peers[peerID] = peer
	}
	peer.Addr = cloneUDPAddr(addr)
	peer.LastSeen = time.Now()
	peer.KeepAliveReceived++

	nowConnected, wasConnected, handler := s.noteConnectionStateLocked()
	s.mu.Unlock()

	s.notifyConnectionState(nowConnected, wasConnected, handler)
}

// noteConnectionStateLocked updates repeaterConnected from the current
// peer count and returns the new/old state plus the handler to notify.
// Must be called with s.mu held; the actual notification is done by the
// caller after unlocking (notifyConnectionState), so peerStateHandler is
// never invoked while holding s.mu.
func (s *IPSCServer) noteConnectionStateLocked() (nowConnected, wasConnected bool, handler func(bool)) {
	nowConnected = len(s.peers) > 0
	wasConnected = s.repeaterConnected
	s.repeaterConnected = nowConnected
	return nowConnected, wasConnected, s.peerStateHandler
}

// notifyConnectionState invokes handler when the connected state actually
// changed. Split out from noteConnectionStateLocked so it always runs
// without s.mu held.
func (s *IPSCServer) notifyConnectionState(nowConnected, wasConnected bool, handler func(bool)) {
	if nowConnected == wasConnected {
		return
	}
	s.verboseLog("IPSC repeater connection state changed", "connected", nowConnected)
	if handler != nil {
		handler(nowConnected)
	}
}

func (s *IPSCServer) buildMasterRegisterReply() []byte {
	packet := make([]byte, 0, 1+4+5+2+4)
	packet = append(packet, byte(PacketType_MasterRegisterReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)

	numPeers := s.peerCount()
	if numPeers > math.MaxUint16 {
		numPeers = math.MaxUint16
	}
	packet = append(packet, uint16ToBytes(uint16(numPeers))...) //nolint:gosec // Bounds checked
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildMasterAliveReply() []byte {
	packet := make([]byte, 0, 1+4+5+4)
	packet = append(packet, byte(PacketType_MasterAliveReply))
	packet = append(packet, s.localIDBytes()...)
	packet = append(packet, s.defaultModeByte())
	flags := s.defaultFlagsBytes()
	packet = append(packet, flags[:]...)
	packet = append(packet, ipscVersion...)
	return packet
}

func (s *IPSCServer) buildPeerListReply() []byte {
	peerList := s.buildPeerList()
	packet := make([]byte, 0, 1+4+2+len(peerList))
	packet = append(packet, byte(PacketType_PeerListReply))
	packet = append(packet, s.localIDBytes()...)
	if len(peerList) > math.MaxUint16 {
		packet = append(packet, uint16ToBytes(math.MaxUint16)...)
	} else {
		packet = append(packet, uint16ToBytes(uint16(len(peerList)))...) //nolint:gosec // Bounds checked
	}
	packet = append(packet, peerList...)
	return packet
}

func (s *IPSCServer) buildPeerList() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.peers) == 0 {
		return nil
	}

	peerList := make([]byte, 0, len(s.peers)*11)
	for _, peer := range s.peers {
		if peer.Addr == nil || peer.Addr.IP == nil {
			continue
		} //nolint:gosec
		peerList = append(peerList, uint32ToBytes(peer.ID)...)
		peerList = append(peerList, peer.Addr.IP.To4()...)
		peerPort := peer.Addr.Port
		if peerPort < 0 || peerPort > 65535 {
			peerPort = 0
		}
		peerList = append(peerList, uint16ToBytes(uint16(peerPort))...) //nolint:gosec // Bounds checked
		peerList = append(peerList, peer.Mode)
	}

	return peerList
}

func (s *IPSCServer) peerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

func (s *IPSCServer) localIDBytes() []byte {
	return uint32ToBytes(s.localID)
}

func (s *IPSCServer) defaultModeByte() byte {
	const (
		peerOperational = 0b01000000
		peerDigital     = 0b00100000
		ts1On           = 0b00001000
		ts2On           = 0b00000010
	)
	return peerOperational | peerDigital | ts1On | ts2On
}

func (s *IPSCServer) defaultFlagsBytes() [4]byte {
	flags := [4]byte{}
	flags[2] = 0x00
	flags[3] = 0x0D
	if s.cfg.IPSC.Auth.Enabled {
		flags[3] |= 0x10
	}
	return flags
}

func parsePeerID(data []byte) (uint32, error) {
	if len(data) < 5 {
		return 0, fmt.Errorf("packet too short for peer ID")
	}
	return binary.BigEndian.Uint32(data[1:5]), nil
}

func uint16ToBytes(value uint16) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, value)
	return buf
}

func uint32ToBytes(value uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, value)
	return buf
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	cloned := &net.UDPAddr{Port: addr.Port, Zone: addr.Zone}
	if addr.IP != nil {
		cloned.IP = append([]byte(nil), addr.IP...)
	}
	return cloned
}

func (s *IPSCServer) auth(data []byte) bool {
	// Last 10 bytes are the sha hash
	payload := data[:len(data)-10]
	hash := data[len(data)-10:]
	expectedHash := hmac.New(sha1.New, s.authKey)
	expectedHash.Write(payload)
	expectedHashSum := expectedHash.Sum(nil)[:10]

	return hmac.Equal(hash, expectedHashSum)
}

func (s *IPSCServer) sendPacket(packet *Packet, addr *net.UDPAddr) error {
	if s.cfg.IPSC.Auth.Enabled {
		hash := hmac.New(sha1.New, s.authKey)
		hash.Write(packet.data)
		hashSum := hash.Sum(nil)[:10]
		packet.data = append(packet.data, hashSum...)
	}

	n, err := s.udp.WriteToUDP(packet.data, addr)
	if err != nil {
		if s.metrics != nil {
			s.metrics.IPSCUDPErrors.WithLabelValues("write").Inc()
		}
		return fmt.Errorf("error sending packet: %w", err)
	}
	if n != len(packet.data) {
		return fmt.Errorf("error sending packet: only sent %d of %d bytes", n, len(packet.data))
	}
	return nil
}

func (s *IPSCServer) SendUserPacket(data []byte) {
	if s.stopped.Load() {
		return
	}
	s.mu.RLock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		if peer.Addr != nil {
			peers = append(peers, peer)
		}
	}
	s.mu.RUnlock()

	for _, peer := range peers {
		s.pacePeer(peer.ID)
		packetData := make([]byte, len(data))
		copy(packetData, data)
		packet := &Packet{data: packetData}
		slog.Debug("IPSC burst sending", "peer", peer.Addr, "length", len(packet.data))
		if err := s.sendPacket(packet, peer.Addr); err != nil {
			slog.Warn("failed sending IPSC user packet", "peer", peer.Addr, "error", err)
		} else if s.metrics != nil {
			s.metrics.IPSCPacketsSent.Inc()
		}
	}
}

func (s *IPSCServer) pacePeer(peerID uint32) {
	const burstInterval = 30 * time.Millisecond

	s.mu.Lock()
	last := s.lastSend[peerID]
	now := time.Now()
	if !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < burstInterval {
			s.mu.Unlock()
			time.Sleep(burstInterval - elapsed)
			s.mu.Lock()
		}
	}
	s.lastSend[peerID] = time.Now()
	s.mu.Unlock()
}