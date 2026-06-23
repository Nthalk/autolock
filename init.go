package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runInit is an interactive wizard that probes HA, helps pick a lock, and writes
// an initial config.yaml.
func runInit(configPath string) error {
	in := bufio.NewReader(os.Stdin)

	if _, err := os.Stat(configPath); err == nil {
		if !askYesNo(in, configPath+" exists. Overwrite?", false) {
			fmt.Println("aborted.")
			return nil
		}
	}

	fmt.Println("airlock setup wizard")
	fmt.Println("Home Assistant connection:")
	url := askValid(in, "  HA URL", "https://homeassistant.local:8123", func(s string) error {
		if !strings.HasPrefix(s, "http") {
			return fmt.Errorf("must start with http(s)://")
		}
		return nil
	})
	fmt.Println("  To get a token:")
	fmt.Printf("    1. Open %s and log in\n", strings.TrimRight(url, "/")+"/profile/security")
	fmt.Println("       (or: click your name at the bottom-left, then the Security tab)")
	fmt.Println("    2. Under \"Long-Lived Access Tokens\", click \"Create Token\"")
	fmt.Println("    3. Name it \"airlock\" and copy it -- HA shows it only once")
	token := askValid(in, "  Paste HA token", "", func(s string) error {
		if s == "" {
			return fmt.Errorf("required")
		}
		return nil
	})

	ha := NewHA(url, token)
	reg := BuildRegistry(nil)
	entity, driver := discoverLock(in, ha, reg)

	slot := askValid(in, "  Code slot for this listing", "2", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return fmt.Errorf("enter a slot number >= 1")
		}
		return nil
	})

	fmt.Println("Airbnb calendar:")
	fmt.Println("  To get the iCal export URL:")
	fmt.Println("    1. airbnb.com -> Menu -> Listings -> select your listing")
	fmt.Println("    2. Calendar -> Availability -> \"Connect to another website\"")
	fmt.Println("       -> \"Export Calendar\"")
	fmt.Println("    3. Copy the .ics link it shows")
	ical := askValid(in, "  Paste iCal URL", "", func(s string) error {
		if !strings.HasPrefix(s, "http") {
			return fmt.Errorf("required; paste the .ics export URL")
		}
		return nil
	})
	if res, err := fetch(ical); err != nil {
		fmt.Printf("  ! could not read calendar yet: %v (saving anyway)\n", err)
	} else {
		fmt.Printf("  found %d reservation(s)\n", len(res))
	}

	fmt.Println("Timing:")
	durValidate := func(s string) error {
		_, err := time.ParseDuration(s)
		return err
	}
	before := askValid(in, "  Set code how long before check-in", "24h", durValidate)
	after := askValid(in, "  Clear code how long after checkout", "0h", durValidate)

	mode := "log"
	if askYesNo(in, "Perform real HA calls now (mode: run)? No = log only", false) {
		mode = "run"
	}

	cfg := renderConfig(url, token, mode, before, after, ical, slot, driver, entity)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n", configPath)
	fmt.Println("next: airlock -setup   (verify)   then   airlock -list")
	return nil
}

// discoverLock probes HA for lock entities and returns the chosen entity and
// driver. Falls back to manual entry when HA is unreachable or has no locks.
func discoverLock(in *bufio.Reader, ha *HA, reg Registry) (entity, driver string) {
	manual := func() (string, string) {
		e := askValid(in, "  Lock entity_id", "lock.front_door", func(s string) error {
			if !strings.HasPrefix(s, "lock.") {
				return fmt.Errorf("expected a lock.* entity_id")
			}
			return nil
		})
		d := askDriver(in, reg, "")
		return e, d
	}

	if st, err := ha.Ping(); err != nil || st != 200 {
		fmt.Printf("  ! HA not reachable (%s); enter lock details manually.\n", pingMsg(st, err))
		return manual()
	}
	fmt.Println("  HA reachable.")

	svc, err := ha.Services()
	if err != nil {
		fmt.Printf("  ! could not list services: %v\n", err)
		return manual()
	}
	states, err := ha.States()
	if err != nil {
		fmt.Printf("  ! could not list states: %v\n", err)
		return manual()
	}

	var available []string
	for _, name := range reg.Names() {
		if reg[name].Available(svc) {
			available = append(available, name)
		}
	}

	var locks []Entity
	for _, e := range states {
		if strings.HasPrefix(e.EntityID, "lock.") {
			locks = append(locks, e)
		}
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].EntityID < locks[j].EntityID })

	if len(locks) == 0 {
		fmt.Println("  ! no lock entities found in HA; enter manually.")
		return manual()
	}

	fmt.Printf("  found %d lock(s):\n", len(locks))
	for i, e := range locks {
		guess := reg.Guess(e, available)
		if guess == "" {
			guess = "?"
		}
		fmt.Printf("    %d) %-40s driver=%-9s %q\n", i+1, e.EntityID, guess, e.FriendlyName())
	}
	idx := askValid(in, "  Select lock", "1", func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > len(locks) {
			return fmt.Errorf("enter 1-%d", len(locks))
		}
		return nil
	})
	n, _ := strconv.Atoi(idx)
	chosen := locks[n-1]
	return chosen.EntityID, askDriver(in, reg, reg.Guess(chosen, available))
}

func askDriver(in *bufio.Reader, reg Registry, def string) string {
	if def == "" {
		def = reg.Names()[0]
	}
	return askValid(in, fmt.Sprintf("  Lock driver (%s)", strings.Join(reg.Names(), ", ")), def, func(s string) error {
		if _, ok := reg[s]; !ok {
			return fmt.Errorf("unknown driver %q", s)
		}
		return nil
	})
}

func pingMsg(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	if status == 401 {
		return "401 - token rejected"
	}
	return fmt.Sprintf("HTTP %d", status)
}

func renderConfig(url, token, mode, before, after, ical, slot, driver, entity string) string {
	return fmt.Sprintf(`# airlock config -- single file, holds secrets. Gitignored.

ha:
  url: %s
  token: %s

# mode: log -> only prints HA calls (safe while a guest stays); run -> performs them
mode: %s

setCodeBeforeCheckin: %s
clearCodeAfterCheckout: %s

# Built-in drivers: zwave_js, zha. Add or override a lock model under `+"`drivers:`"+`
# (see config.yaml.example). Run `+"`airlock -locks`"+` to list lock entities.
calendars:
  - url: %s
    slot: %s
    lock:
      driver: %s
      entity: %s
`, url, token, mode, before, after, ical, slot, driver, entity)
}

func ask(r *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askValid(r *bufio.Reader, prompt, def string, validate func(string) error) string {
	for {
		v := ask(r, prompt, def)
		if err := validate(v); err != nil {
			fmt.Printf("    %v\n", err)
			continue
		}
		return v
	}
}

func askYesNo(r *bufio.Reader, prompt string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	fmt.Printf("%s [%s]: ", prompt, d)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}
