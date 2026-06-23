package main

import (
	"fmt"
	"net/http"
	"strings"
)

// setup diagnoses configuration and connectivity and prints an actionable
// checklist. It never performs lock calls.
func setup(configPath string) error {
	var fails int
	check := func(ok bool, msg, hint string) {
		mark := "x"
		if ok {
			mark = "ok"
		} else {
			fails++
		}
		fmt.Printf("  [%s] %s\n", mark, msg)
		if !ok && hint != "" {
			fmt.Printf("        -> %s\n", hint)
		}
	}
	warn := func(msg, hint string) {
		fmt.Printf("  [!] %s\n", msg)
		if hint != "" {
			fmt.Printf("        -> %s\n", hint)
		}
	}

	fmt.Println("airlock setup")

	// --- config ---
	fmt.Println("config.yaml")
	cfg, cerr := LoadConfig(configPath)
	check(cerr == nil, configPath+" parses",
		fmt.Sprintf("cp config.yaml.example %s and fill it in -- %s", configPath, errHint(cerr)))
	if cerr != nil {
		return fmt.Errorf("%d problem(s) found", fails)
	}
	reg := BuildRegistry(cfg.Drivers)
	ha := NewHA(cfg.HA.URL, cfg.HA.Token)

	check(ha.BaseURL != "", "ha.url set", "set ha.url: https://ha.iodesystems.com")
	check(ha.Token != "", "ha.token set", "HA -> Profile -> Security -> Long-Lived Access Tokens")

	verr := cfg.Validate(reg)
	check(verr == nil, "config valid", errHint(verr))
	if cfg.Mode == "log" {
		warn("mode: log -- calls will be printed, not performed", "set mode: run to go live")
	}

	// --- calendars ---
	fmt.Println("calendars")
	for i, cal := range cfg.Calendars {
		res, err := fetch(cal.URL)
		check(err == nil, fmt.Sprintf("calendar #%d fetched & parsed", i+1), errHint(err))
		if err == nil {
			fmt.Printf("        %d reservation(s), slot %d, driver %s, entity %s\n",
				len(res), cal.Slot, cal.Lock.Driver, cal.Lock.Entity)
		}
	}

	// --- Home Assistant ---
	fmt.Println("home assistant")
	if ha.BaseURL == "" || ha.Token == "" {
		warn("skipping HA probes (ha.url/ha.token missing)", "")
		if fails > 0 {
			return fmt.Errorf("%d problem(s) found", fails)
		}
		return nil
	}

	status, err := ha.Ping()
	switch {
	case err != nil:
		check(false, "HA reachable", errHint(err))
	case status == http.StatusUnauthorized:
		check(false, "HA token accepted", "token rejected (401) -- regenerate ha.token")
	case status != http.StatusOK:
		check(false, "HA API ok", fmt.Sprintf("GET /api/ returned %d", status))
	default:
		check(true, "HA reachable & token accepted", "")

		svc, serr := ha.Services()
		check(serr == nil, "HA services listed", errHint(serr))
		states, sterr := ha.States()
		check(sterr == nil, "HA states listed", errHint(sterr))

		// Per-calendar driver availability + entity existence.
		locks := map[string]Entity{}
		for _, e := range states {
			if strings.HasPrefix(e.EntityID, "lock.") {
				locks[e.EntityID] = e
			}
		}
		for i, cal := range cfg.Calendars {
			d, known := reg[cal.Lock.Driver]
			if !known {
				continue // already reported by Validate
			}
			if serr == nil {
				check(d.Available(svc),
					fmt.Sprintf("calendar #%d driver %q available in HA", i+1, cal.Lock.Driver),
					fmt.Sprintf("integration not loaded; available drivers: %s", strings.Join(availableDrivers(reg, svc), ", ")))
			}
			if sterr == nil {
				_, ok := locks[cal.Lock.Entity]
				check(ok, fmt.Sprintf("calendar #%d entity %s exists", i+1, cal.Lock.Entity),
					"run: airlock -locks to list lock entities")
			}
		}
	}

	if fails > 0 {
		return fmt.Errorf("%d problem(s) found", fails)
	}
	fmt.Println("all checks passed")
	return nil
}

// availableDrivers lists registered drivers whose service is present on HA.
func availableDrivers(reg Registry, svc Services) []string {
	var out []string
	for _, name := range reg.Names() {
		if reg[name].Available(svc) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}

func errHint(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
