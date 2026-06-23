package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration parsed from a YAML string like "24h" or "30m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// LockSpec binds a calendar to a lock driver and entity.
type LockSpec struct {
	Driver string `yaml:"driver"` // registered driver name, e.g. zwave_js
	Entity string `yaml:"entity"` // HA entity_id, e.g. lock.front_door
}

// Calendar is an Airbnb iCal feed bound to a lock and a code slot.
type Calendar struct {
	URL  string   `yaml:"url"`
	Slot int      `yaml:"slot"`
	Lock LockSpec `yaml:"lock"`
}

// HASpec is the Home Assistant connection.
type HASpec struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// Config is the airlock configuration (single file; holds secrets).
type Config struct {
	HA HASpec `yaml:"ha"`
	// Mode: "run" performs HA calls; "log" only prints them (safe while a
	// guest is in residence). Defaults to "run".
	Mode string `yaml:"mode"`
	// SetCodeBeforeCheckin: program the code this long before check-in.
	SetCodeBeforeCheckin Duration `yaml:"setCodeBeforeCheckin"`
	// ClearCodeAfterCheckout: clear the code this long after checkout.
	ClearCodeAfterCheckout Duration `yaml:"clearCodeAfterCheckout"`
	// Drivers adds or overrides lock-command specs by name (see BuiltinDrivers).
	Drivers   map[string]DriverSpec `yaml:"drivers"`
	Calendars []Calendar            `yaml:"calendars"`
}

// LoadConfig parses the YAML config (no validation; see Validate).
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the config is runnable: at least one calendar, each with a
// known driver, an entity, and a positive slot.
func (c *Config) Validate(reg Registry) error {
	if len(c.Calendars) == 0 {
		return fmt.Errorf("no calendars configured")
	}
	for i, cal := range c.Calendars {
		_, known := reg[cal.Lock.Driver]
		switch {
		case cal.Lock.Driver == "":
			return fmt.Errorf("calendar #%d: lock.driver is required (one of %s)", i+1, strings.Join(reg.Names(), ", "))
		case !known:
			return fmt.Errorf("calendar #%d: unknown lock.driver %q (have %s)", i+1, cal.Lock.Driver, strings.Join(reg.Names(), ", "))
		case cal.Lock.Entity == "":
			return fmt.Errorf("calendar #%d: lock.entity is required", i+1)
		case cal.Slot <= 0:
			return fmt.Errorf("calendar #%d: slot must be >= 1", i+1)
		}
	}
	return nil
}
