# airlock

Reads an Airbnb hosting iCal feed and programs a Home Assistant smart lock around
each reservation — set the guest's code before check-in, clear it after checkout.
The door code is the last 4 digits of the guest's phone number, which Airbnb
publishes in the calendar feed.

airlock calls Home Assistant's REST API directly. No shell, no `curl`, no
hand-written HA scripts — the lock integration call is built by a Go driver.

## How it works

1. Fetch each iCal feed (URL from `config.yaml`).
2. Parse guest reservations: confirmation code, phone last-4, check-in/checkout
   dates. (Host "Not available" blocks are ignored.)
3. For each reservation, plan two actions — set-code (before check-in) and
   clear-code (after checkout) — and perform any whose target falls in the
   current window via the calendar's lock driver.

Run it from cron; it's a one-shot that acts only on due actions.

## Install

No clone needed (requires Go 1.21+):

```sh
go install github.com/Nthalk/autolock@latest   # builds the `autolock` binary into $GOBIN
autolock -init                                 # then run from any directory
```

Or run it one-off without installing:

```sh
go run github.com/Nthalk/autolock@latest -init
```

Both read/write `config.yaml` (and `airlock.state`) in the current directory; run
from a dedicated folder. The rest of this README uses `go run .` for local
development — substitute `autolock` once installed.

## Setup

Easiest path — the wizard probes HA, lets you pick the lock, and writes
`config.yaml` for you (with step-by-step instructions for where to find the HA
token and the Airbnb iCal URL):

```sh
go run . -init
```

Everything lives in one gitignored `config.yaml` (it holds the HA token and
calendar URLs). To set it up by hand instead:

```sh
cp config.yaml.example config.yaml
```

Fill in:

- `ha.url` — `https://ha.iodesystems.com`
- `ha.token` — HA → Profile → Security → **Long-Lived Access Tokens** → Create.
- `calendars[].url` — Airbnb → Calendar → Availability → *Connect to another
  website* → export `.ics` URL.

Then find your lock:

```sh
go run . -locks
```

Lists every `lock.*` entity in HA and which driver can program it, plus a ready
`lock:` block to paste into a calendar. Supported drivers: `zwave_js`, `zha`.

```yaml
ha:
  url: https://ha.iodesystems.com
  token: eyJ...

mode: log                  # log = print only; run = perform calls
setCodeBeforeCheckin: 24h  # program code this long before check-in
clearCodeAfterCheckout: 0h # clear code this long after checkout

calendars:
  - url: https://www.airbnb.com/calendar/ical/XXXX.ics?t=XXXX
    slot: 2                 # lock code slot for this listing
    lock:
      driver: zwave_js
      entity: lock.touchscreen_deadbolt_z_wave_plus
```

Each iCal feed binds to one lock + code slot, so multiple listings each get their
own slot.

`mode: log` prints the HA calls it *would* make without performing them — keep it
there while a guest is staying. Switch to `mode: run` to go live.

## Usage

```sh
go run . -init             # interactive wizard; writes config.yaml
go run . -locks            # discover compatible lock entities
go run . -setup            # diagnose secrets, calendars, HA, drivers, entities
go run . -list             # show parsed reservations
go run . -all -dry-run     # print every planned call, ignore timing
go run . -all              # same as dry-run while mode: log; bypasses state
go run .                   # perform actions due within -window, exactly once
go build -o airlock .      # build a binary
```

## Cron

```cron
# hourly; -window should be >= the cron interval so no action is missed
0 * * * * cd /path/to/airlock && ./airlock >> airlock.log 2>&1
```

Each performed action is recorded in `airlock.state` (override with `-state`,
disable with `-state ""`). A given action — keyed by reservation, type, target
time, and exact call — fires **exactly once**, so running cron more often than
`-window` is safe, and a changed reservation date or code re-fires. `-window`
only sets how far back a still-pending action is allowed to catch up. Entries
older than 90 days are pruned on load. `-all` bypasses both timing and state to
force every action (useful for testing).

## Drivers

A driver is just a pair of lock commands described as data — the HA service to
call and its payload — so a lock model with a different command structure is
added in config, no code. Built-ins: `zwave_js`, `zha` (see `BuiltinDrivers` in
`lock.go`). Define or override under `drivers:`:

```yaml
drivers:
  acme_lock:
    match: [acme]                 # optional; helps -locks guess this driver
    set:
      service: acme.program_code  # domain.service
      data: {lock: __ENTITY__, index: __SLOT__, pin: __CODE__}
    clear:
      service: acme.erase_code
      data: {lock: __ENTITY__, index: __SLOT__}
```

Tokens filled at call time: `__ENTITY__` (lock entity_id), `__SLOT__` (code
slot — emitted as a number when it's the whole value, else inline), `__CODE__`
(guest phone last 4). A driver is "available" when its `set` service is
registered in HA, which is what `-setup` and `-locks` check.
