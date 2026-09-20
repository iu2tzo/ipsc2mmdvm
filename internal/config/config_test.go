package config

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

// validConfig returns a minimal Config that passes all validation checks
// that don't depend on OS state (netlink). Because Validate() calls
// netlink.LinkByName we can only exercise the checks that run *before*
// the interface lookup or the auth-key regex check.
func validConfig() Config {
	return Config{
		LogLevel: LogLevelInfo,
		MMDVM: []MMDVM{
			{
				Name:         "BM",
				Callsign:     "N0CALL",
				ID:           12345,
				ColorCode:    7,
				Latitude:     30.0,
				Longitude:    -97.0,
				MasterServer: "master.example.com:62030",
				Password:     "password",
			},
		},
		IPSC: IPSC{
			Interface:  "lo", // loopback exists on all Linux hosts
			Port:       50000,
			IP:         "10.10.250.1",
			SubnetMask: 24,
			Auth: IPSCAuth{
				Enabled: false,
			},
		},
	}
}

func TestValidateLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		level    LogLevel
		wantErr  error
		hasError bool
	}{
		{"debug", LogLevelDebug, nil, false},
		{"verbose", LogLevelVerbose, nil, false},
		{"info", LogLevelInfo, nil, false},
		{"warn", LogLevelWarn, nil, false},
		{"error", LogLevelError, nil, false},
		{"invalid", LogLevel("trace"), ErrInvalidLogLevel, true},
		{"empty", LogLevel(""), ErrInvalidLogLevel, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.LogLevel = tt.level
			err := c.Validate()
			if tt.hasError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
			}
			// Valid levels may still fail on netlink lookup,
			// so we only assert that the log-level error is NOT returned.
			if !tt.hasError && errors.Is(err, ErrInvalidLogLevel) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidLogLevel, err)
			}
		})
	}
}

func TestValidateMMDVMCallsign(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].Callsign = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMCallsign) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMCallsign, err)
	}
}

func TestValidateMMDVMColorCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cc      uint8
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid 15", 15, false},
		{"invalid 16", 16, true},
		{"invalid 255", 255, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].ColorCode = tt.cc
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMColorCode, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMColorCode) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMColorCode)
			}
		})
	}
}

func TestValidateMMDVMLatitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lat     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -90", -90, false},
		{"valid 90", 90, false},
		{"invalid -91", -91, true},
		{"invalid 91", 91, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].Latitude = tt.lat
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLatitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLatitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLatitude)
			}
		})
	}
}

func TestValidateMMDVMLongitude(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lng     float64
		wantErr bool
	}{
		{"valid 0", 0, false},
		{"valid -180", -180, false},
		{"valid 180", 180, false},
		{"invalid -181", -181, true},
		{"invalid 181", 181, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.MMDVM[0].Longitude = tt.lng
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("expected %v, got %v", ErrInvalidMMDVMLongitude, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMMDVMLongitude) {
				t.Fatalf("did not expect %v", ErrInvalidMMDVMLongitude)
			}
		})
	}
}

func TestValidateMMDVMMasterServer(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].MasterServer = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMMasterServer) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMMasterServer, err)
	}
}

func TestValidateMMDVMPassword(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.MMDVM[0].Password = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidMMDVMPassword) {
		t.Fatalf("expected %v, got %v", ErrInvalidMMDVMPassword, err)
	}
}

func TestIPSCMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interfac string
		ip       string
		wantMode IPSCMode
	}{
		{"managed: interface + real ip", "eth0", "10.10.250.1", IPSCModeManaged},
		{"any: no interface, no ip", "", "", IPSCModeAny},
		{"any: no interface, explicit 0.0.0.0", "", "0.0.0.0", IPSCModeAny},
		{"bind-device: interface, empty ip", "wg0", "", IPSCModeBindDevice},
		{"bind-device: interface, explicit 0.0.0.0", "wg0", "0.0.0.0", IPSCModeBindDevice},
		{"invalid: ip without interface", "", "10.10.250.1", IPSCModeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := IPSC{Interface: tt.interfac, IP: tt.ip}
			if got := c.Mode(); got != tt.wantMode {
				t.Fatalf("expected mode %v, got %v", tt.wantMode, got)
			}
		})
	}
}

func TestValidateIPSCInterfaceManagedMode(t *testing.T) {
	t.Parallel()
	// interface + ip both set (Managed mode): a nonexistent
	// interface must still be rejected.
	c := validConfig()
	c.IPSC.Interface = "this-interface-does-not-exist-hopefully"
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCInterface) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCInterface, err)
	}
}

func TestValidateIPSCAnyMode(t *testing.T) {
	t.Parallel()
	// Neither interface nor ip: listen everywhere, no error expected
	// from this part of validation.
	c := validConfig()
	c.IPSC.Interface = ""
	c.IPSC.IP = ""
	err := c.Validate()
	if errors.Is(err, ErrInvalidIPSCInterface) || errors.Is(err, ErrInvalidIPSCIP) ||
		errors.Is(err, ErrInvalidIPSCSubnetMask) || errors.Is(err, ErrInvalidIPSCConfiguration) {
		t.Fatalf("did not expect an IPSC validation error in Any mode, got %v", err)
	}
}

func TestValidateIPSCBindDeviceMode(t *testing.T) {
	t.Parallel()
	// Interface set, ip empty: bind to the interface only.
	// "lo" exists on any Linux host.
	c := validConfig()
	c.IPSC.Interface = "lo"
	c.IPSC.IP = ""
	err := c.Validate()
	if errors.Is(err, ErrInvalidIPSCInterface) || errors.Is(err, ErrInvalidIPSCConfiguration) {
		t.Fatalf("did not expect an IPSC validation error in BindDevice mode, got %v", err)
	}
}

func TestValidateIPSCBindDeviceModeMissingInterface(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Interface = "this-interface-does-not-exist-hopefully"
	c.IPSC.IP = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCInterface) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCInterface, err)
	}
}

func TestValidateIPSCInvalidConfiguration(t *testing.T) {
	t.Parallel()
	// Specific IP without an interface to assign it to: ambiguous
	// combination, must be explicitly rejected.
	c := validConfig()
	c.IPSC.Interface = ""
	c.IPSC.IP = "10.10.250.1"
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCConfiguration) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCConfiguration, err)
	}
}

func TestValidateIPSCPort(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Port = 0
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCPort) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCPort, err)
	}
}

func TestValidateIPSCSubnetMask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mask    int
		wantErr bool
	}{
		{"valid 1", 1, false},
		{"valid 24", 24, false},
		{"valid 32", 32, false},
		{"invalid 0", 0, true},
		{"invalid 33", 33, true},
		{"invalid -1", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.IPSC.SubnetMask = tt.mask
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidIPSCSubnetMask) {
				t.Fatalf("expected %v, got %v", ErrInvalidIPSCSubnetMask, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidIPSCSubnetMask) {
				t.Fatalf("did not expect %v", ErrInvalidIPSCSubnetMask)
			}
		})
	}
}

func TestValidateIPSCAuthKeyRequired(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyBadHex(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "ZZZZ" // Not valid hex
	err := c.Validate()
	if !errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("expected %v, got %v", ErrInvalidIPSCAuthKey, err)
	}
}

func TestValidateIPSCAuthKeyValid(t *testing.T) {
	t.Parallel()
	c := validConfig()
	c.IPSC.Auth.Enabled = true
	c.IPSC.Auth.Key = "deadbeef"
	err := c.Validate()
	// Should not fail on auth key validation itself
	if errors.Is(err, ErrInvalidIPSCAuthKey) {
		t.Fatalf("did not expect %v", ErrInvalidIPSCAuthKey)
	}
}

func TestLogLevelConstants(t *testing.T) {
	t.Parallel()
	if LogLevelDebug != "debug" {
		t.Fatalf("expected 'debug', got %q", LogLevelDebug)
	}
	if LogLevelInfo != "info" {
		t.Fatalf("expected 'info', got %q", LogLevelInfo)
	}
	if LogLevelWarn != "warn" {
		t.Fatalf("expected 'warn', got %q", LogLevelWarn)
	}
	if LogLevelError != "error" {
		t.Fatalf("expected 'error', got %q", LogLevelError)
	}
}

func TestValidateMetricsAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"default port only", ":9100", false},
		{"host and port", "0.0.0.0:9100", false},
		{"localhost", "127.0.0.1:2112", false},
		{"empty disables", "", false},
		{"missing port", "localhost", true},
		{"invalid format", ":::bad", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validConfig()
			c.Metrics.Address = tt.addr
			err := c.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("expected %v, got %v", ErrInvalidMetricsAddress, err)
			}
			if !tt.wantErr && errors.Is(err, ErrInvalidMetricsAddress) {
				t.Fatalf("did not expect %v, got %v", ErrInvalidMetricsAddress, err)
			}
		})
	}
}

// TestDefaultRepeaterTimeoutMatchesConstant guards against the
// `default:"90"` struct tag on IPSC.RepeaterTimeout drifting out of sync
// with DefaultRepeaterTimeoutSeconds: struct tags are compile-time string
// literals, so the tag can't reference the constant directly, and this is
// the only automated check tying the two together.
func TestDefaultRepeaterTimeoutMatchesConstant(t *testing.T) {
	t.Parallel()

	field, ok := reflect.TypeOf(IPSC{}).FieldByName("RepeaterTimeout")
	if !ok {
		t.Fatal("IPSC.RepeaterTimeout field not found")
	}

	tagDefault, err := strconv.ParseUint(field.Tag.Get("default"), 10, 64)
	if err != nil {
		t.Fatalf("failed to parse `default` tag on IPSC.RepeaterTimeout: %v", err)
	}

	if uint(tagDefault) != DefaultRepeaterTimeoutSeconds {
		t.Fatalf("IPSC.RepeaterTimeout `default` tag (%d) does not match DefaultRepeaterTimeoutSeconds (%d)",
			tagDefault, DefaultRepeaterTimeoutSeconds)
	}
}

func TestLogsConnectionFlow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level LogLevel
		want  bool
	}{
		{LogLevelDebug, true},
		{LogLevelVerbose, true},
		{LogLevelInfo, false},
		{LogLevelWarn, false},
		{LogLevelError, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			c := Config{LogLevel: tt.level}
			if got := c.LogsConnectionFlow(); got != tt.want {
				t.Fatalf("LogsConnectionFlow() with level %q = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}