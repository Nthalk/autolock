package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Reservation is a single guest booking parsed from the Airbnb iCal feed.
type Reservation struct {
	Code     string    // confirmation code, e.g. HMWCKBJHSH
	Last4    string    // last 4 digits of guest phone, e.g. 3341
	CheckIn  time.Time // DTSTART
	CheckOut time.Time // DTEND (checkout day, exclusive of last night)
	Slot     int       // lock code slot, from the calendar this came from
}

var (
	reCode  = regexp.MustCompile(`details/([A-Za-z0-9]+)`)
	reLast4 = regexp.MustCompile(`Last 4 Digits\)\s*:\s*(\d{4})`)
)

// unfold reads an iCal stream and applies RFC 5545 line unfolding: a line that
// begins with a space or tab is a continuation of the previous line.
func unfold(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var lines []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += line[1:]
		} else {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

// splitProp splits an iCal content line into its property name (parameters
// stripped) and value.
func splitProp(line string) (name, val string) {
	name, val, ok := strings.Cut(line, ":")
	if !ok {
		return "", ""
	}
	name, _, _ = strings.Cut(name, ";")
	return name, val
}

// parseDate handles both VALUE=DATE (20060102) and date-time forms.
func parseDate(v string) (time.Time, error) {
	v = strings.TrimSuffix(v, "Z")
	for _, layout := range []string{"20060102T150405", "20060102"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", v)
}

// ParseICS extracts guest reservations. "Not available" host blocks carry no
// reservation data and are skipped.
func ParseICS(r io.Reader) ([]Reservation, error) {
	lines, err := unfold(r)
	if err != nil {
		return nil, err
	}
	var res []Reservation
	var summary, desc, dtstart, dtend string
	inEvent := false
	for _, line := range lines {
		switch {
		case line == "BEGIN:VEVENT":
			inEvent, summary, desc, dtstart, dtend = true, "", "", "", ""
		case line == "END:VEVENT":
			inEvent = false
			if !strings.HasPrefix(summary, "Reserved") {
				continue
			}
			r := Reservation{}
			if m := reCode.FindStringSubmatch(desc); m != nil {
				r.Code = m[1]
			}
			if m := reLast4.FindStringSubmatch(desc); m != nil {
				r.Last4 = m[1]
			}
			if r.CheckIn, err = parseDate(dtstart); err != nil {
				return nil, fmt.Errorf("reservation %s: DTSTART: %w", r.Code, err)
			}
			if r.CheckOut, err = parseDate(dtend); err != nil {
				return nil, fmt.Errorf("reservation %s: DTEND: %w", r.Code, err)
			}
			res = append(res, r)
		case inEvent:
			switch name, val := splitProp(line); name {
			case "SUMMARY":
				summary = val
			case "DESCRIPTION":
				desc = val
			case "DTSTART":
				dtstart = val
			case "DTEND":
				dtend = val
			}
		}
	}
	return res, nil
}
