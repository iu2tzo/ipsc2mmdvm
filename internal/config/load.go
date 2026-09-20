package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// ConfigFileFlag is the name of the flag used to override the config file path.
const ConfigFileFlag = "config"

// leaf is a scalar (non-struct, non-struct-slice) field discovered while
// walking the Config struct, together with the env var / flag names derived
// from its `name` tag.
type leaf struct {
	value    reflect.Value
	def      string
	desc     string
	envName  string
	flagName string
}

// walkLeaves recursively visits every scalar field of v, which must be a
// struct. Fields without a `name` tag are skipped, as are fields that are
// slices of structs (e.g. Config.MMDVM, MMDVM.TGRewrites): those can only
// be expressed in a YAML config file, not as an env var or a flag.
func walkLeaves(v reflect.Value, envPrefix, flagPrefix string, fn func(leaf) error) error {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		name := field.Tag.Get("name")
		if name == "" {
			continue
		}
		fv := v.Field(i)

		switch {
		case fv.Kind() == reflect.Struct:
			if err := walkLeaves(fv, envPrefix+field.Name+"_", flagPrefix+name+".", fn); err != nil {
				return err
			}
		case (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array) && field.Type.Elem().Kind() == reflect.Struct:
			continue
		default:
			envName := strings.ToUpper(strings.ReplaceAll(envPrefix+name, "-", "_"))
			l := leaf{
				value:    fv,
				def:      field.Tag.Get("default"),
				desc:     field.Tag.Get("description"),
				envName:  envName,
				flagName: flagPrefix + name,
			}
			if err := fn(l); err != nil {
				return err
			}
		}
	}
	return nil
}

// setFromString parses s according to v's kind and sets v to the result.
func setFromString(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("invalid bool %q: %w", s, err)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", s, err)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q: %w", s, err)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("invalid float %q: %w", s, err)
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("unsupported config field type %s", v.Kind())
	}
	return nil
}

// applyDefaults sets every leaf field that has a `default` tag.
func applyDefaults(v reflect.Value) error {
	return walkLeaves(v, "", "", func(l leaf) error {
		if l.def == "" {
			return nil
		}
		return setFromString(l.value, l.def)
	})
}

// applyEnv overlays values found in matching environment variables, e.g.
// IPSC.Auth.Key -> IPSC_AUTH_KEY.
func applyEnv(v reflect.Value) error {
	return walkLeaves(v, "", "", func(l leaf) error {
		val, ok := os.LookupEnv(l.envName)
		if !ok {
			return nil
		}
		return setFromString(l.value, val)
	})
}

// applyFlags overlays values from explicitly-set flags, e.g. IPSC.Auth.Key
// -> --ipsc.auth.key.
func applyFlags(v reflect.Value, flags *pflag.FlagSet) error {
	return walkLeaves(v, "", "", func(l leaf) error {
		f := flags.Lookup(l.flagName)
		if f == nil || !f.Changed {
			return nil
		}
		return setFromString(l.value, f.Value.String())
	})
}

// RegisterFlags registers one pflag per scalar Config field (e.g.
// --log-level, --ipsc.port, --ipsc.auth.key), plus -c/--config to override
// the config file path. Call it before the flag set is parsed.
func RegisterFlags(flags *pflag.FlagSet, defaultConfigPath string) {
	flags.StringP(ConfigFileFlag, "c", defaultConfigPath, "config file")

	var cfg Config
	v := reflect.ValueOf(&cfg).Elem()
	_ = walkLeaves(v, "", "", func(l leaf) error {
		registerFlag(flags, l)
		return nil
	})
}

//nolint:gocyclo
func registerFlag(flags *pflag.FlagSet, l leaf) {
	switch l.value.Kind() {
	case reflect.String:
		flags.String(l.flagName, l.def, l.desc)
	case reflect.Bool:
		b, _ := strconv.ParseBool(l.def)
		flags.Bool(l.flagName, b, l.desc)
	case reflect.Int:
		n, _ := strconv.ParseInt(l.def, 10, 64)
		flags.Int(l.flagName, int(n), l.desc)
	case reflect.Int8:
		n, _ := strconv.ParseInt(l.def, 10, 8)
		flags.Int8(l.flagName, int8(n), l.desc)
	case reflect.Int16:
		n, _ := strconv.ParseInt(l.def, 10, 16)
		flags.Int16(l.flagName, int16(n), l.desc)
	case reflect.Int32:
		n, _ := strconv.ParseInt(l.def, 10, 32)
		flags.Int32(l.flagName, int32(n), l.desc)
	case reflect.Int64:
		n, _ := strconv.ParseInt(l.def, 10, 64)
		flags.Int64(l.flagName, n, l.desc)
	case reflect.Uint:
		n, _ := strconv.ParseUint(l.def, 10, 64)
		flags.Uint(l.flagName, uint(n), l.desc)
	case reflect.Uint8:
		n, _ := strconv.ParseUint(l.def, 10, 8)
		flags.Uint8(l.flagName, uint8(n), l.desc)
	case reflect.Uint16:
		n, _ := strconv.ParseUint(l.def, 10, 16)
		flags.Uint16(l.flagName, uint16(n), l.desc)
	case reflect.Uint32:
		n, _ := strconv.ParseUint(l.def, 10, 32)
		flags.Uint32(l.flagName, uint32(n), l.desc)
	case reflect.Uint64:
		n, _ := strconv.ParseUint(l.def, 10, 64)
		flags.Uint64(l.flagName, n, l.desc)
	case reflect.Float32:
		f, _ := strconv.ParseFloat(l.def, 32)
		flags.Float32(l.flagName, float32(f), l.desc)
	case reflect.Float64:
		f, _ := strconv.ParseFloat(l.def, 64)
		flags.Float64(l.flagName, f, l.desc)
	}
}

// Load builds a Config by layering, in increasing order of priority:
// struct-tag defaults, the YAML config file, environment variables, and
// explicitly-set flags. It then validates the result.
func Load(flags *pflag.FlagSet, defaultConfigPath string) (*Config, error) {
	cfg := &Config{}
	v := reflect.ValueOf(cfg).Elem()

	if err := applyDefaults(v); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %w", err)
	}

	configPath := defaultConfigPath
	if flags != nil && flags.Changed(ConfigFileFlag) {
		if p, err := flags.GetString(ConfigFileFlag); err == nil {
			configPath = p
		}
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse config file %q: %w", configPath, err)
			}
		case os.IsNotExist(err):
			// No config file: fall back to defaults/env/flags.
		default:
			return nil, fmt.Errorf("failed to read config file %q: %w", configPath, err)
		}
	}

	if err := applyEnv(v); err != nil {
		return nil, fmt.Errorf("failed to apply environment variables: %w", err)
	}

	if flags != nil {
		if err := applyFlags(v, flags); err != nil {
			return nil, fmt.Errorf("failed to apply flags: %w", err)
		}
	}

	return cfg, cfg.Validate()
}
