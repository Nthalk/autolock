package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
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
		window     = flag.Duration("window", time.Hour, "fire an action if its target falls within the last <window> (also the daemon check cadence)")
		refresh    = flag.Duration("refresh", 12*time.Hour, "daemon: how often to re-fetch the iCal feeds")
		daemon     = flag.Bool("daemon", false, "run continuously: refresh calendars and act on due actions")
		statePath  = flag.String("state", "airlock.state", "file recording fired actions for exactly-once (empty to disable)")
		lockPath   = flag.String("lock", "airlock.lock", "lock file preventing concurrent runs (empty to disable)")
		install    = flag.String("install", "", "print a service definition and exit: systemd | initd | cron")
	)
	flag.Parse()

	if *doInit {
		return runInit(*configPath)
	}
	if *doSetup {
		return setup(*configPath)
	}
	if *install != "" {
		return printService(*install, *configPath)
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

	if *list {
		bookings, err := fetchBookings(cfg)
		if err != nil {
			return err
		}
		for _, b := range bookings {
			fmt.Printf("%-12s slot=%d last4=%s  %s -> %s  [%s %s]\n",
				b.res.Code, b.res.Slot, b.res.Last4,
				b.res.CheckIn.Format("2006-01-02"), b.res.CheckOut.Format("2006-01-02"),
				b.cal.Lock.Driver, b.cal.Lock.Entity)
		}
		return nil
	}

	if *lockPath != "" {
		release, err := acquireLock(*lockPath)
		if err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				fmt.Println("airlock: another instance is running; exiting")
				return nil
			}
			return fmt.Errorf("lock: %w", err)
		}
		defer release()
	}

	rt := &runtime{cfg: cfg, reg: reg, ha: ha, state: state, window: *window, all: *all, logOnly: logOnly}
	if *daemon {
		return serve(rt, *refresh)
	}

	bookings, err := fetchBookings(cfg)
	if err != nil {
		return err
	}
	if n := rt.evaluate(bookings, time.Now()); n > 0 {
		return fmt.Errorf("%d call(s) failed", n)
	}
	return nil
}

// booking pairs a reservation with the calendar (lock/slot) it came from.
type booking struct {
	res Reservation
	cal Calendar
}

func fetchBookings(cfg *Config) ([]booking, error) {
	var out []booking
	for _, cal := range cfg.Calendars {
		res, err := fetch(cal.URL)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			r.Slot = cal.Slot
			out = append(out, booking{res: r, cal: cal})
		}
	}
	return out, nil
}

// runtime bundles everything an evaluation pass needs.
type runtime struct {
	cfg     *Config
	reg     Registry
	ha      *HA
	state   *State
	window  time.Duration
	all     bool
	logOnly bool
}

// evaluate performs (or logs) every action due as of now, returning the failure
// count.
func (rt *runtime) evaluate(bookings []booking, now time.Time) int {
	var failures int
	for _, b := range bookings {
		for _, a := range plan(rt.cfg, b.res, b.cal, rt.reg) {
			due := rt.all || (!now.Before(a.Target) && now.Before(a.Target.Add(rt.window)))
			if !due {
				continue
			}
			tag := fmt.Sprintf("%s %s", b.res.Code, a.Label)
			if rt.logOnly {
				fmt.Printf("[%s] @%s would call: %s\n", tag, a.Target.Format("2006-01-02 15:04"), a.Call)
				continue
			}

			key := strings.Join([]string{b.res.Code, a.Label, a.Target.Format(time.RFC3339), a.Call.String()}, "|")
			if !rt.all && rt.state.Has(key) {
				continue // already fired
			}

			fmt.Printf("[%s] @%s calling: %s\n", tag, a.Target.Format("2006-01-02 15:04"), a.Call)
			if err := rt.ha.CallService(a.Call); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] call failed: %v\n", tag, err)
				failures++
				continue
			}
			if err := rt.state.Mark(key, a.Target); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] warning: could not record state: %v\n", tag, err)
			}
		}
	}
	return failures
}

// serve runs the daemon loop: refresh the feeds every refresh, evaluate due
// actions every window, until interrupted.
func serve(rt *runtime, refresh time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("airlock daemon: refresh=%s check=%s mode=%s\n", refresh, rt.window,
		map[bool]string{true: "log", false: "run"}[rt.logOnly])

	var bookings []booking
	var lastFetch time.Time
	refetch := func() {
		bs, err := fetchBookings(rt.cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "refresh failed: %v (keeping %d cached)\n", err, len(bookings))
			return
		}
		bookings, lastFetch = bs, time.Now()
		fmt.Printf("refreshed: %d reservation(s)\n", len(bookings))
	}

	refetch()
	rt.evaluate(bookings, time.Now())

	ticker := time.NewTicker(rt.window)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("airlock daemon: shutting down")
			return nil
		case <-ticker.C:
			if lastFetch.IsZero() || time.Since(lastFetch) >= refresh {
				refetch()
			}
			rt.evaluate(bookings, time.Now())
		}
	}
}

// acquireLock takes an exclusive non-blocking flock; the returned func releases
// it. EWOULDBLOCK means another instance holds it.
func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
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
