package config

import (
	"errors"
	"net"
	"regexp"

	"github.com/vishvananda/netlink"
)

type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	// LogLevelVerbose sits between debug and info: it enables extra
	// connection-flow logging (IPSC listener/peer lifecycle, MMDVM
	// connect/reconnect attempts, ...) without the much higher-volume
	// per-packet logging that "debug" also turns on. See
	// Config.LogsConnectionFlow().
	LogLevelVerbose LogLevel = "verbose"
	LogLevelInfo    LogLevel = "info"
	LogLevelWarn    LogLevel = "warn"
	LogLevelError   LogLevel = "error"
)

// DefaultRepeaterTimeoutSeconds is the single source of truth for the
// IPSC.RepeaterTimeout default. Struct tags are compile-time literals, so
// the `default:"90"` tag on IPSC.RepeaterTimeout below can't reference this
// constant directly — TestDefaultRepeaterTimeoutMatchesConstant in
// config_test.go asserts the two stay in sync. Callers that construct an
// ipsc.IPSCServer directly, bypassing Load()/Validate() (e.g. tests), get
// this same value as a fallback when RepeaterTimeout is zero.
const DefaultRepeaterTimeoutSeconds uint = 90

type Config struct {
	LogLevel LogLevel `name:"log-level" yaml:"log-level" description:"Logging level for the application. One of debug, verbose, info, warn, or error" default:"info"`
	Metrics  Metrics  `name:"metrics" yaml:"metrics" description:"Configuration for Prometheus metrics"`
	MMDVM    []MMDVM  `name:"mmdvm" yaml:"mmdvm" description:"Configuration for MMDVM clients (multiple DMR masters)"`
	IPSC     IPSC     `name:"ipsc" yaml:"ipsc" description:"Configuration for the IPSC server"`
}

// LogsConnectionFlow reports whether extra connection-flow logging
// (IPSC listener/peer lifecycle, MMDVM connect/reconnect attempts, ...)
// should be emitted: true for both LogLevelVerbose and LogLevelDebug,
// since "debug" is a superset of "verbose".
func (c Config) LogsConnectionFlow() bool {
	return c.LogLevel == LogLevelVerbose || c.LogLevel == LogLevelDebug
}

type Metrics struct {
	Enabled bool   `name:"enabled" yaml:"enabled" description:"Whether to enable Prometheus metrics endpoint"`
	Address string `name:"address" yaml:"address" description:"Address to serve Prometheus metrics on" default:":9100"`
}

// IPSC configures how the IPSC server listens for repeater traffic.
// Depending on which fields are set, it operates in one of three modes
// (see Mode()):
//
//   - Managed: interface + a real ip (not "0.0.0.0"/empty) are both set.
//     The interface's addressing is fully managed via netlink (existing
//     addresses removed, ip/subnet-mask assigned, link brought up). This
//     is the original behaviour, for a repeater on a dedicated/direct
//     link.
//
//   - Any: ip is "0.0.0.0" or empty, and interface is empty. Listens on
//     the configured port on every interface, no address management.
//
//   - BindDevice: ip is "0.0.0.0" or empty, and interface is set. Binds
//     the listening socket to that specific interface (SO_BINDTODEVICE)
//     without touching its existing address(es).
//
// NOTE: ip intentionally has no default value. A default here would
// make "interface omitted from the config file" indistinguishable from
// "interface explicitly set", which is exactly the distinction Mode()
// needs to make between the three listed modes.
type IPSC struct {
	Interface  string   `name:"interface" yaml:"interface" description:"Interface to listen for IPSC packets on. Leave empty to listen on all interfaces (ip=0.0.0.0/empty) or to bind to a specific interface without managing its address (ip=0.0.0.0/empty + interface set)"`
	Port       uint16   `name:"port" yaml:"port" description:"Port to listen for IPSC packets on"`
	IP         string   `name:"ip" yaml:"ip" description:"IP address to assign to the interface (managed mode), or \"0.0.0.0\"/empty to listen without managing addressing"`
	SubnetMask int      `name:"subnet-mask" yaml:"subnet-mask" description:"Subnet mask for the virtual network interface created for IPSC packets (managed mode only)" default:"24"`
	Auth       IPSCAuth `name:"auth" yaml:"auth" description:"Authentication configuration for the IPSC server"`

	// RequireRepeater/RepeaterTimeout gate the MMDVM (DMR network) connections
	// on repeater presence. See MMDVMClient lifecycle in cmd/root.go for how
	// these are consumed.
	RequireRepeater bool `name:"require-repeater" yaml:"require-repeater" description:"When enabled, connections to the configured DMR network servers are only opened while a repeater is registered with the IPSC server, and closed again once the repeater disconnects. When disabled (default), DMR network connections are opened immediately at startup regardless of repeater state"`
	// RepeaterTimeout's default below (90) must match DefaultRepeaterTimeoutSeconds;
	// struct tags are compile-time literals so it can't reference the constant
	// directly (enforced by TestDefaultRepeaterTimeoutMatchesConstant).
	RepeaterTimeout uint `name:"repeater-timeout" yaml:"repeater-timeout" description:"Seconds of inactivity after which a registered repeater is considered disconnected. Only used when require-repeater is enabled" default:"90"`
}

// IPSCMode identifies how the IPSC server should open its listening
// socket, derived from IPSC.Interface / IPSC.IP. Shared by
// config.Validate() and ipsc.IPSCServer.Start() so both always agree on
// what a given configuration means.
type IPSCMode int

const (
	// IPSCModeInvalid: the combination of fields doesn't map to any
	// supported mode (e.g. a specific ip given without an interface to
	// assign it to).
	IPSCModeInvalid IPSCMode = iota

	// IPSCModeManaged: interface + a real ip set -> full netlink-managed
	// addressing (original/legacy behaviour, direct-cable repeater).
	IPSCModeManaged

	// IPSCModeAny: no interface, no specific ip -> listen on the
	// configured port on every interface.
	IPSCModeAny

	// IPSCModeBindDevice: interface set, no specific ip -> bind the
	// socket to that interface (SO_BINDTODEVICE), address untouched.
	IPSCModeBindDevice
)

// Mode derives the IPSC listening mode from Interface and IP.
func (c IPSC) Mode() IPSCMode {
	anyIP := c.IP == "" || c.IP == "0.0.0.0"

	switch {
	case c.Interface != "" && !anyIP:
		return IPSCModeManaged
	case c.Interface != "" && anyIP:
		return IPSCModeBindDevice
	case c.Interface == "" && anyIP:
		return IPSCModeAny
	default:
		// c.Interface == "" && !anyIP: a specific ip with nothing to
		// assign it to. Ambiguous, rejected by Validate().
		return IPSCModeInvalid
	}
}

type IPSCAuth struct {
	Enabled bool   `name:"enabled" yaml:"enabled" description:"Whether to require authentication for IPSC clients"`
	Key     string `name:"key" yaml:"key" description:"Authentication key for IPSC clients. Required if auth is enabled"`
}

type MMDVM struct {
	Name     string `name:"name" yaml:"name" description:"Name for this MMDVM network (used in logging)"`
	Callsign string `name:"callsign" yaml:"callsign" description:"Callsign to use for the MMDVM connection"`
	ID       uint32 `name:"radio-id" yaml:"radio-id" description:"Radio ID for the MMDVM connection"`
	// RXFreq is in Hz
	RXFreq uint `name:"rx-freq" yaml:"rx-freq" description:"Receive frequency in Hz for the MMDVM connection"`
	// TXFreq is in Hz
	TXFreq uint `name:"tx-freq" yaml:"tx-freq" description:"Transmit frequency in Hz for the MMDVM connection"`
	// TXPower is in dBm
	TXPower uint8 `name:"tx-power" yaml:"tx-power" description:"Transmit power in dBm for the MMDVM connection"`
	// ColorCode is the DMR color code
	ColorCode uint8 `name:"color-code" yaml:"color-code" description:"DMR color code for the MMDVM connection"`
	// Latitude with north as positive [-90,+90]
	Latitude float64 `name:"latitude" yaml:"latitude" description:"Latitude with north as positive [-90,+90] for the MMDVM connection"`
	// Longitude with east as positive [-180+,180]
	Longitude float64 `name:"longitude" yaml:"longitude" description:"Longitude with east as positive [-180+,180] for the MMDVM connection"`
	// Height in meters
	Height       uint16 `name:"height" yaml:"height" description:"Height in meters for the MMDVM connection"`
	Location     string `name:"location" yaml:"location" description:"Location for the MMDVM connection"`
	Description  string `name:"description" yaml:"description" description:"Description for the MMDVM connection"`
	URL          string `name:"url" yaml:"url" description:"URL for the MMDVM connection"`
	Slots        byte   `name:"slots" yaml:"slots" description:"Active timeslots bitmask (1=TS1, 2=TS2, 3=both)" default:"3"`
	MasterServer string `name:"master-server" yaml:"master-server" description:"Master server for the MMDVM connection"`
	Password     string `name:"password" yaml:"password" description:"Password for the MMDVM connection"`

	// Rewrite rules for routing DMR data to/from this network.
	TGRewrites   []TGRewriteConfig   `name:"tg-rewrite" yaml:"tg-rewrite" description:"Talkgroup rewrite rules"`
	PCRewrites   []PCRewriteConfig   `name:"pc-rewrite" yaml:"pc-rewrite" description:"Private call rewrite rules"`
	TypeRewrites []TypeRewriteConfig `name:"type-rewrite" yaml:"type-rewrite" description:"Type rewrite rules (group TG to private call)"`
	SrcRewrites  []SrcRewriteConfig  `name:"src-rewrite" yaml:"src-rewrite" description:"Source rewrite rules (private call by source to group TG)"`

	// PassAll rules allow all traffic of a given type on a slot without rewriting.
	PassAllPC []int `name:"pass-all-pc" yaml:"pass-all-pc" description:"Timeslots on which all private calls pass through unchanged (e.g. [1, 2])"`
	PassAllTG []int `name:"pass-all-tg" yaml:"pass-all-tg" description:"Timeslots on which all group calls pass through unchanged (e.g. [1, 2])"`
}

// TGRewriteConfig maps group TG calls from one slot/TG to another.
// Modeled after DMRGateway's TGRewrite: fromSlot, fromTG, toSlot, toTG, range.
type TGRewriteConfig struct {
	FromSlot uint `name:"from-slot" yaml:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" yaml:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" yaml:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToTG     uint `name:"to-tg" yaml:"to-tg" description:"Destination talkgroup start"`
	Range    uint `name:"range" yaml:"range" description:"Number of contiguous TGs to map" default:"1"`
}

// PCRewriteConfig maps private calls from one slot/ID to another.
// Modeled after DMRGateway's PCRewrite: fromSlot, fromId, toSlot, toId, range.
type PCRewriteConfig struct {
	FromSlot uint `name:"from-slot" yaml:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" yaml:"from-id" description:"Source private call ID start"`
	ToSlot   uint `name:"to-slot" yaml:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" yaml:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" yaml:"range" description:"Number of contiguous IDs to map" default:"1"`
}

// TypeRewriteConfig converts group TG calls to private calls.
// Modeled after DMRGateway's TypeRewrite: fromSlot, fromTG, toSlot, toId, range.
type TypeRewriteConfig struct {
	FromSlot uint `name:"from-slot" yaml:"from-slot" description:"Source timeslot (1 or 2)"`
	FromTG   uint `name:"from-tg" yaml:"from-tg" description:"Source talkgroup start"`
	ToSlot   uint `name:"to-slot" yaml:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" yaml:"to-id" description:"Destination private call ID start"`
	Range    uint `name:"range" yaml:"range" description:"Number of contiguous entries to map" default:"1"`
}

// SrcRewriteConfig matches calls by source ID and remaps the source into a prefixed range.
type SrcRewriteConfig struct {
	FromSlot uint `name:"from-slot" yaml:"from-slot" description:"Source timeslot (1 or 2)"`
	FromID   uint `name:"from-id" yaml:"from-id" description:"Source ID start"`
	ToSlot   uint `name:"to-slot" yaml:"to-slot" description:"Destination timeslot (1 or 2)"`
	ToID     uint `name:"to-id" yaml:"to-id" description:"Destination source ID start"`
	Range    uint `name:"range" yaml:"range" description:"Number of contiguous source IDs to match" default:"1"`
}

var (
	ErrInvalidLogLevel          = errors.New("invalid log level provided")
	ErrNoMMDVMNetworks          = errors.New("at least one MMDVM network must be configured")
	ErrInvalidMMDVMName         = errors.New("invalid MMDVM network name provided")
	ErrDuplicateMMDVMName       = errors.New("duplicate MMDVM network name provided")
	ErrInvalidMMDVMCallsign     = errors.New("invalid MMDVM callsign provided")
	ErrInvalidMMDVMColorCode    = errors.New("invalid MMDVM color code provided")
	ErrInvalidMMDVMLongitude    = errors.New("invalid MMDVM longitude provided")
	ErrInvalidMMDVMLatitude     = errors.New("invalid MMDVM latitude provided")
	ErrInvalidMMDVMMasterServer = errors.New("invalid MMDVM master server provided")
	ErrInvalidMMDVMPassword     = errors.New("invalid MMDVM password provided")
	ErrInvalidRewriteSlot       = errors.New("invalid rewrite slot (must be 1 or 2)")
	ErrInvalidRewriteRange      = errors.New("invalid rewrite range (must be >= 1)")
	ErrInvalidIPSCInterface     = errors.New("invalid IPSC interface provided")
	ErrInvalidIPSCIP            = errors.New("invalid IPSC IP address provided")
	ErrInvalidIPSCSubnetMask    = errors.New("invalid IPSC subnet mask provided")
	ErrInvalidIPSCPort          = errors.New("invalid IPSC port provided")
	ErrInvalidIPSCConfiguration = errors.New(
		"ambiguous IPSC configuration: use interface+ip for managed mode, " +
			"ip=\"0.0.0.0\"/empty alone to listen on all interfaces, " +
			"or interface alone (ip empty) to bind to that interface",
	)
	ErrInvalidIPSCAuthKey         = errors.New("invalid IPSC authentication key provided")
	ErrInvalidMetricsAddress      = errors.New("invalid metrics address provided")
	ErrInvalidIPSCRepeaterTimeout = errors.New("invalid IPSC repeater timeout: must be greater than zero when require-repeater is enabled")
)

func (c Config) Validate() error {
	switch c.LogLevel {
	case LogLevelDebug, LogLevelVerbose, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return ErrInvalidLogLevel
	}

	if c.Metrics.Enabled && c.Metrics.Address != "" {
		_, _, err := net.SplitHostPort(c.Metrics.Address)
		if err != nil {
			return ErrInvalidMetricsAddress
		}
	}

	if len(c.MMDVM) == 0 {
		return ErrNoMMDVMNetworks
	}

	names := make(map[string]struct{}, len(c.MMDVM))
	for i := range c.MMDVM {
		h := &c.MMDVM[i]

		// Default name to "Network N" if empty
		if h.Name == "" {
			return ErrInvalidMMDVMName
		}

		if _, ok := names[h.Name]; ok {
			return ErrDuplicateMMDVMName
		}
		names[h.Name] = struct{}{}

		if h.Callsign == "" {
			return ErrInvalidMMDVMCallsign
		}

		if h.ColorCode > 15 {
			return ErrInvalidMMDVMColorCode
		}

		if h.Longitude < -180 || h.Longitude > 180 {
			return ErrInvalidMMDVMLongitude
		}

		if h.Latitude < -90 || h.Latitude > 90 {
			return ErrInvalidMMDVMLatitude
		}

		if h.MasterServer == "" {
			return ErrInvalidMMDVMMasterServer
		}

		if h.Password == "" {
			return ErrInvalidMMDVMPassword
		}

		if err := validateRewrites(h); err != nil {
			return err
		}
	}

	if c.IPSC.Port == 0 {
		return ErrInvalidIPSCPort
	}

	switch c.IPSC.Mode() {
	case IPSCModeManaged:
		if _, err := netlink.LinkByName(c.IPSC.Interface); err != nil {
			return ErrInvalidIPSCInterface
		}
		if net.ParseIP(c.IPSC.IP) == nil {
			return ErrInvalidIPSCIP
		}
		if c.IPSC.SubnetMask < 1 || c.IPSC.SubnetMask > 32 {
			return ErrInvalidIPSCSubnetMask
		}

	case IPSCModeBindDevice:
		// No address management: still verify that the given interface
		// actually exists, so a typo isn't only discovered when the
		// listener starts.
		if _, err := netlink.LinkByName(c.IPSC.Interface); err != nil {
			return ErrInvalidIPSCInterface
		}

	case IPSCModeAny:
		// No interface, no specific IP: listen everywhere, nothing to
		// check beyond the port (already done above).

	default: // IPSCModeInvalid
		return ErrInvalidIPSCConfiguration
	}

	if c.IPSC.RequireRepeater && c.IPSC.RepeaterTimeout == 0 {
		return ErrInvalidIPSCRepeaterTimeout
	}

	if c.IPSC.Auth.Enabled && c.IPSC.Auth.Key == "" {
		return ErrInvalidIPSCAuthKey
	}

	// Check authkey is [0-9a-fA-F]{0,40}
	authKeyRegexp := regexp.MustCompile(`^[0-9a-fA-F]{0,40}$`)
	if !authKeyRegexp.MatchString(c.IPSC.Auth.Key) {
		return ErrInvalidIPSCAuthKey
	}

	return nil
}

func validateSlot(slot uint) bool {
	return slot == 1 || slot == 2
}

func validateRewrites(h *MMDVM) error {
	for _, r := range h.TGRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.PCRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.TypeRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	for _, r := range h.SrcRewrites {
		if !validateSlot(r.FromSlot) || !validateSlot(r.ToSlot) {
			return ErrInvalidRewriteSlot
		}
		if r.Range < 1 {
			return ErrInvalidRewriteRange
		}
	}
	return nil
}