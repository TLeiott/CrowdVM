package node

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule lists the weekly windows in which the node offers compute, in
// the machine's local time. Syntax: entries separated by ";" or newlines,
// each "<days> [HH:MM-HH:MM]", e.g. "mon-fri 19:00-08:00; sat,sun". Days
// are mon..sun, ranges (mon-fri), lists (sat,sun), "daily", "weekdays" and
// "weekends". Without a time the whole day counts; an end at or before the
// start runs past midnight into the next day.
type Schedule []window

type window struct {
	days       [7]bool // indexed by time.Weekday
	start, end int     // minutes since midnight; end 1440 = midnight
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// ParseSchedule parses a schedule ("" = none).
func ParseSchedule(s string) (Schedule, error) {
	var out Schedule
	for _, e := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ';' || r == '\n' }) {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		f := strings.Fields(e)
		if len(f) > 2 {
			return nil, fmt.Errorf("schedule entry %q: want \"<days> [HH:MM-HH:MM]\"", e)
		}
		var w window
		if err := parseDays(f[0], &w.days); err != nil {
			return nil, fmt.Errorf("schedule entry %q: %w", e, err)
		}
		w.end = 24 * 60
		if len(f) == 2 {
			a, b, ok := strings.Cut(f[1], "-")
			st, err1 := parseClock(a)
			en, err2 := parseClock(b)
			if !ok || err1 != nil || err2 != nil {
				return nil, fmt.Errorf("schedule entry %q: time must be HH:MM-HH:MM", e)
			}
			if st == en {
				return nil, fmt.Errorf("schedule entry %q: start and end are the same", e)
			}
			w.start, w.end = st, en
		}
		out = append(out, w)
	}
	return out, nil
}

func parseDays(s string, days *[7]bool) error {
	for _, part := range strings.Split(s, ",") {
		switch part {
		case "daily", "all", "everyday":
			*days = [7]bool{true, true, true, true, true, true, true}
			continue
		case "weekdays":
			for d := 1; d <= 5; d++ {
				days[d] = true
			}
			continue
		case "weekends":
			days[0], days[6] = true, true
			continue
		}
		a, b, isRange := strings.Cut(part, "-")
		from, to := dayIndex(a), dayIndex(b)
		if !isRange {
			to = from
		}
		if from < 0 || to < 0 {
			return fmt.Errorf("unknown day %q (use mon..sun, daily, weekdays, weekends)", part)
		}
		for d := from; ; d = (d + 1) % 7 {
			days[d] = true
			if d == to {
				break
			}
		}
	}
	return nil
}

func dayIndex(s string) int {
	for i, n := range dayNames {
		if strings.HasPrefix(s, n) { // "mon", "monday"
			return i
		}
	}
	return -1
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return hh*60 + mm, nil
}

// Active reports whether t falls in a window.
func (s Schedule) Active(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	d := int(t.Weekday())
	prev := (d + 6) % 7
	for _, w := range s {
		if w.end > w.start {
			if w.days[d] && m >= w.start && m < w.end {
				return true
			}
			continue
		}
		// Past midnight: from start on a listed day until end the day after.
		if (w.days[d] && m >= w.start) || (w.days[prev] && m < w.end) {
			return true
		}
	}
	return false
}

// NextChange is the next minute at which Active flips (zero: never).
func (s Schedule) NextChange(t time.Time) time.Time {
	if len(s) == 0 {
		return time.Time{}
	}
	now := s.Active(t)
	c := t.Truncate(time.Minute)
	for range 8 * 24 * 60 {
		c = c.Add(time.Minute)
		if s.Active(c) != now {
			return c
		}
	}
	return time.Time{}
}

// ScheduleHold overrides the schedule until the next boundary: "offer until
// 08:00" outside the windows, "pause until 19:00" inside one.
type ScheduleHold struct {
	Offer bool      `json:"offer"`
	Until time.Time `json:"until"`
}

// Pause reasons (proto.NodeInfo.PauseReason).
const (
	PauseOwner    = "owner"    // the master switch: paused
	PauseHold     = "hold"     // a schedule hold
	PauseSchedule = "schedule" // outside the schedule's windows
)

// PauseState is the effective pause the node advertises.
type PauseState struct {
	Paused bool      `json:"paused"`
	Reason string    `json:"reason,omitempty"` // set when Paused
	Until  time.Time `json:"until,omitzero"`   // when the hold or schedule next changes it
}

// EffectivePause combines the master switch, a hold and the schedule.
func (c Config) EffectivePause(now time.Time) PauseState {
	if c.Paused {
		return PauseState{Paused: true, Reason: PauseOwner}
	}
	s, err := ParseSchedule(c.Schedule)
	if h := c.ScheduleHold; h != nil && now.Before(h.Until) {
		ps := PauseState{Paused: !h.Offer, Until: h.Until}
		if ps.Paused {
			ps.Reason = PauseHold
		}
		// The hold ends at a boundary where the schedule usually switches
		// to the same state ("offer until 03:00" when the window opens at
		// 03:00): the state really changes at the schedule's next boundary.
		if err == nil && len(s) > 0 && s.Active(h.Until) == h.Offer {
			if next := s.NextChange(h.Until); !next.IsZero() {
				ps.Until = next
			}
		}
		return ps
	}
	if err != nil || len(s) == 0 {
		return PauseState{}
	}
	ps := PauseState{Paused: !s.Active(now), Until: s.NextChange(now)}
	if ps.Paused {
		ps.Reason = PauseSchedule
	}
	return ps
}

// holdFor builds the hold for "offer" or "pause" against the schedule: it
// lasts until the schedule's next boundary.
func (c Config) holdFor(val string, now time.Time) (*ScheduleHold, error) {
	if val == "" || val == "off" || val == "none" {
		return nil, nil
	}
	if val != "offer" && val != "pause" {
		return nil, fmt.Errorf("schedule-hold must be offer, pause or \"\"")
	}
	s, err := ParseSchedule(c.Schedule)
	if err != nil || len(s) == 0 {
		return nil, fmt.Errorf("schedule-hold needs a schedule (crowdvm-node config set schedule ...)")
	}
	next := s.NextChange(now)
	if next.IsZero() {
		return nil, fmt.Errorf("the schedule never changes, so a hold would never end")
	}
	return &ScheduleHold{Offer: val == "offer", Until: next}, nil
}
