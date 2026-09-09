package caldav

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"github.com/emersion/go-ical"
)

// Timezone is a calendar's default timezone, in the form clients expect: an
// iCalendar object holding exactly one VTIMEZONE. The library parses and
// validates; you store the bytes.
//
// The zero value means the calendar has no default timezone, which is a
// perfectly ordinary calendar.
type Timezone struct {
	b []byte
}

// ParseTimezone validates b as a timezone definition and keeps a copy of it.
func ParseTimezone(b []byte) (Timezone, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return Timezone{}, fmt.Errorf("caldav: timezone is not valid iCalendar: %w", err)
	}

	var tz *ical.Component
	for _, comp := range cal.Children {
		if comp.Name != ical.CompTimezone {
			return Timezone{}, fmt.Errorf("caldav: timezone object contains a %s; it must hold only a VTIMEZONE", comp.Name)
		}
		if tz != nil {
			return Timezone{}, fmt.Errorf("caldav: timezone object holds more than one VTIMEZONE")
		}
		tz = comp
	}
	if tz == nil {
		return Timezone{}, fmt.Errorf("caldav: timezone object holds no VTIMEZONE")
	}
	if id, err := tz.Props.Text(ical.PropTimezoneID); err != nil || id == "" {
		return Timezone{}, fmt.Errorf("caldav: VTIMEZONE has no TZID")
	}

	return Timezone{b: slices.Clone(b)}, nil
}

// TimezoneFor derives a timezone definition from a Go location.
//
// It covers locations whose offset never changes — UTC, a fixed offset, a zone
// that does not observe daylight saving. Anywhere that does transition, the
// definition needs the transition rules, which Go does not expose: fetch that
// zone's VTIMEZONE from your tzdata source and use ParseTimezone.
func TimezoneFor(loc *time.Location) (Timezone, error) {
	if loc == nil {
		return Timezone{}, fmt.Errorf("caldav: nil location")
	}

	year := time.Now().In(loc).Year()
	name, offset := time.Date(year, time.January, 1, 0, 0, 0, 0, loc).Zone()
	for month := time.February; month <= time.December; month++ {
		if _, off := time.Date(year, month, 1, 0, 0, 0, 0, loc).Zone(); off != offset {
			return Timezone{}, fmt.Errorf("caldav: %s changes offset during %d; supply its VTIMEZONE to ParseTimezone", loc, year)
		}
	}

	utcOffset := formatUTCOffset(offset)
	std := ical.NewComponent(ical.CompTimezoneStandard)
	std.Props.Set(rawProp("DTSTART", "19700101T000000"))
	std.Props.Set(rawProp("TZNAME", name))
	std.Props.Set(rawProp("TZOFFSETFROM", utcOffset))
	std.Props.Set(rawProp("TZOFFSETTO", utcOffset))

	tz := ical.NewComponent(ical.CompTimezone)
	tz.Props.SetText(ical.PropTimezoneID, loc.String())
	tz.Children = append(tz.Children, std)

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//go-webdav//caldav//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, tz)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return Timezone{}, fmt.Errorf("caldav: encoding timezone for %s: %w", loc, err)
	}
	return Timezone{b: buf.Bytes()}, nil
}

// Bytes returns a copy of the definition.
func (t Timezone) Bytes() []byte { return slices.Clone(t.b) }

// IsZero reports whether the calendar has no default timezone.
func (t Timezone) IsZero() bool { return len(t.b) == 0 }

// rawProp writes a value under its property's own default type. SetText would
// stamp VALUE=TEXT on DTSTART and the TZOFFSET properties, which are DATE-TIME
// and UTC-OFFSET; a client reading that back gets a timezone it cannot use.
func rawProp(name, value string) *ical.Prop {
	prop := ical.NewProp(name)
	prop.Value = value
	return prop
}

// formatUTCOffset writes an RFC 5545 UTC-offset value. Seconds are emitted
// only when the offset actually has them, as several pre-1972 zones do:
// Africa/Monrovia ran at -00:44:30, and truncating that to -00:44 moves every
// reading before its 1972 changeover by half a minute.
func formatUTCOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}

	if remainder := seconds % 60; remainder != 0 {
		return fmt.Sprintf("%s%02d%02d%02d", sign, seconds/3600, (seconds%3600)/60, remainder)
	}

	return fmt.Sprintf("%s%02d%02d", sign, seconds/3600, (seconds%3600)/60)
}
