package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "airlock:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "config.yaml", "path to config")
		dryRun     = flag.Bool("dry-run", false, "print the HA calls instead of performing them")
		all        = flag.Bool("all", false, "ignore timing; act on every action of every reservation")
		list       = flag.Bool("list", false, "list reservations and exit")
		doInit     = flag.Bool("init", false, "interactive wizard to create config.yaml, then exit")
		doSetup    = flag.Bool("setup", false, "diagnose configuration and connectivity, then exit")
		doLocks    = flag.Bool("locks", false, "search HA for compatible locks, then exit")
		window     = flag.Duration("window", time.Hour, "fire an action if its target falls within the last <window> (set to your cron interval)")
		statePath  = flag.String("state", "airlock.state", "file recording fired actions for exactly-once (empty to disable)")
	)
	flag.Parse()

	if *doInit {
		return runInit(*configPath)
	}
	if *doSetup {
		return setup(*configPath)
	}

	// Best-effort load so -locks sees config-defined drivers even if the rest of
	// the config is incomplete.
	cfg, cfgErr := LoadConfig(*configPath)
	reg := BuildRegistry(driversFrom(cfg))
	ha := NewHA(haURL(cfg), haToken(cfg))

	if *doLocks {
		return searchLocks(ha, reg)
	}

	if cfgErr != nil {
		return cfgErr
	}
	if err := cfg.Validate(reg); err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}

	logOnly := *dryRun || cfg.Mode == "log"
	if !logOnly && (ha.BaseURL == "" || ha.Token == "") {
		return fmt.Errorf("mode: run requires ha.url and ha.token in %s", *configPath)
	}

	state, err := LoadState(*statePath)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	now := time.Now()
	var failures int
	for _, cal := range cfg.Calendars {
		res, err := fetch(cal.URL)
		if err != nil {
			return err
		}
		for _, r := range res {
			r.Slot = cal.Slot

			if *list {
				fmt.Printf("%-12s slot=%d last4=%s  %s -> %s  [%s %s]\n",
					r.Code, r.Slot, r.Last4,
					r.CheckIn.Format("2006-01-02"), r.CheckOut.Format("2006-01-02"),
					cal.Lock.Driver, cal.Lock.Entity)
				continue
			}

			for _, a := range plan(cfg, r, cal, reg) {
				due := *all || (!now.Before(a.Target) && now.Before(a.Target.Add(*window)))
				if !due {
					continue
				}
				tag := fmt.Sprintf("%s %s", r.Code, a.Label)
				if logOnly {
					fmt.Printf("[%s] @%s would call: %s\n", tag, a.Target.Format("2006-01-02 15:04"), a.Call)
					continue
				}

				key := strings.Join([]string{r.Code, a.Label, a.Target.Format(time.RFC3339), a.Call.String()}, "|")
				if !*all && state.Has(key) {
					continue // already fired
				}

				fmt.Printf("[%s] @%s calling: %s\n", tag, a.Target.Format("2006-01-02 15:04"), a.Call)
				if err := ha.CallService(a.Call); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] call failed: %v\n", tag, err)
					failures++
					continue
				}
				if err := state.Mark(key, a.Target); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] warning: could not record state: %v\n", tag, err)
				}
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d call(s) failed", failures)
	}
	return nil
}

// Action is a dated HA call for one reservation.
type Action struct {
	Label  string
	Target time.Time
	Call   ServiceCall
}

func driversFrom(cfg *Config) map[string]DriverSpec {
	if cfg == nil {
		return nil
	}
	return cfg.Drivers
}

func haURL(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.HA.URL
}

func haToken(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.HA.Token
}

// plan resolves a reservation into its set-code and clear-code actions.
func plan(cfg *Config, r Reservation, cal Calendar, reg Registry) []Action {
	d := reg[cal.Lock.Driver]
	return []Action{
		{
			Label:  "checkin set-code",
			Target: r.CheckIn.Add(-time.Duration(cfg.SetCodeBeforeCheckin)),
			Call:   d.SetCode(cal.Lock.Entity, r.Slot, r.Last4),
		},
		{
			Label:  "checkout clear-code",
			Target: r.CheckOut.Add(time.Duration(cfg.ClearCodeAfterCheckout)),
			Call:   d.ClearCode(cal.Lock.Entity, r.Slot),
		},
	}
}

// searchLocks lists lock entities and which drivers can program them.
func searchLocks(ha *HA, reg Registry) error {
	if ha.BaseURL == "" || ha.Token == "" {
		return fmt.Errorf("set ha.url and ha.token in config.yaml to search for locks")
	}
	svc, err := ha.Services()
	if err != nil {
		return err
	}
	states, err := ha.States()
	if err != nil {
		return err
	}

	var available []string
	for _, name := range reg.Names() {
		if reg[name].Available(svc) {
			available = append(available, name)
		}
	}
	fmt.Printf("compatible drivers available on HA: %s\n", orNone(available))

	var locks []Entity
	for _, e := range states {
		if strings.HasPrefix(e.EntityID, "lock.") {
			locks = append(locks, e)
		}
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].EntityID < locks[j].EntityID })

	fmt.Printf("lock entities (%d):\n", len(locks))
	for _, e := range locks {
		guess := reg.Guess(e, available)
		if guess == "" {
			guess = "?"
		}
		fmt.Printf("  %-40s %-10s driver=%-9s %q\n", e.EntityID, e.State, guess, e.FriendlyName())
	}
	if len(locks) > 0 && len(available) > 0 {
		e := locks[0]
		driver := reg.Guess(e, available)
		if driver == "" {
			driver = available[0]
		}
		fmt.Printf("\nadd to a calendar in config.yaml:\n  lock:\n    driver: %s\n    entity: %s\n", driver, e.EntityID)
		fmt.Println("(driver is a best guess from the entity name; verify it matches the lock's integration.)")
	}
	return nil
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func fetch(url string) ([]Reservation, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	return ParseICS(resp.Body)
}
