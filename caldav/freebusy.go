package caldav

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mniehe/davkit/internal"
)

// busyPeriod is one contribution to a free-busy report: a clipped interval and
// the FBTYPE it is reported under, where the empty string is plain BUSY.
type busyPeriod struct {
	start, end time.Time
	fbType     string
}

// reportFreeBusy serves the CALDAV:free-busy-query REPORT (RFC 4791 §7.10).
// The response is a single synthesised VFREEBUSY, never the items themselves,
// which is why ViewAvailability suffices to run it.
func (a *adapter) reportFreeBusy(w http.ResponseWriter, r *http.Request, acc access, fb *freeBusyQuery) error {
	if fb.TimeRange == nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: free-busy-query requires a time-range")
	}
	start := time.Time(fb.TimeRange.Start).UTC()
	end := time.Time(fb.TimeRange.End).UTC()
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: free-busy-query time-range must run from a start to a later end")
	}

	ctx := r.Context()
	items, _, err := a.listItems(ctx, acc.CalendarRef())
	if err != nil {
		return backendError(err)
	}

	window := calendarExpandRequest{Start: start, End: end}
	budget := newExpansionBudget()
	var periods []busyPeriod
	for i := range items {
		data, parseErr := ical.NewDecoder(bytes.NewReader(items[i].Content)).Decode()
		if parseErr != nil {
			return fmt.Errorf("caldav: item %q holds bytes that do not parse as iCalendar: %w", items[i].Name, parseErr)
		}
		expanded, expandErr := expandCalendarWithin(data, &window, budget, forBusyTime)
		if expandErr != nil {
			return expandErr
		}
		for _, comp := range expanded.Children {
			found, busyErr := busyPeriodsOf(comp, start, end)
			if busyErr != nil {
				return fmt.Errorf("caldav: item %q: %w", items[i].Name, busyErr)
			}
			periods = append(periods, found...)
		}
	}

	cal, err := freeBusyCalendar(start, end, coalesce(periods))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if encodeErr := ical.NewEncoder(&buf).Encode(cal); encodeErr != nil {
		return fmt.Errorf("caldav: encoding free-busy response: %w", encodeErr)
	}
	w.Header().Set("Content-Type", calendarMediaType)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(buf.Bytes())
	return err
}

// busyPeriodsOf extracts the busy time one expanded component contributes
// within [start, end), per the RFC 4791 §7.10 rules: opaque, uncancelled
// events count (tentative ones as BUSY-TENTATIVE), stored VFREEBUSY periods
// count unless marked FREE, and nothing else counts at all.
func busyPeriodsOf(comp *ical.Component, start, end time.Time) ([]busyPeriod, error) {
	switch comp.Name {
	case ical.CompEvent:
		if valueIs(comp, ical.PropTransparency, "TRANSPARENT") || valueIs(comp, ical.PropStatus, "CANCELLED") {
			return nil, nil
		}
		from, to, err := compInterval(comp, time.UTC)
		if err != nil {
			return nil, err
		}
		p, ok := clip(busyPeriod{start: from, end: to}, start, end)
		if !ok {
			return nil, nil
		}
		if valueIs(comp, ical.PropStatus, "TENTATIVE") {
			p.fbType = "BUSY-TENTATIVE"
		}
		return []busyPeriod{p}, nil
	case ical.CompFreeBusy:
		var periods []busyPeriod
		for _, prop := range comp.Props[ical.PropFreeBusy] {
			fbType := strings.ToUpper(prop.Params.Get("FBTYPE"))
			if fbType == "FREE" {
				continue
			}
			if fbType == "BUSY" {
				fbType = ""
			}
			for _, value := range strings.Split(prop.Value, ",") {
				from, to, err := period(value, time.UTC)
				if err != nil {
					return nil, err
				}
				if p, ok := clip(busyPeriod{start: from, end: to, fbType: fbType}, start, end); ok {
					periods = append(periods, p)
				}
			}
		}
		return periods, nil
	default:
		return nil, nil
	}
}

func valueIs(comp *ical.Component, name, want string) bool {
	prop := comp.Props.Get(name)
	return prop != nil && strings.EqualFold(prop.Value, want)
}

func clip(p busyPeriod, start, end time.Time) (busyPeriod, bool) {
	if p.start.Before(start) {
		p.start = start
	}
	if p.end.After(end) {
		p.end = end
	}
	if !p.start.Before(p.end) {
		return busyPeriod{}, false
	}
	return p, true
}

// coalesce merges overlapping or touching periods of the same FBTYPE, sorted
// ascending, as RFC 5545 §3.8.2.6 asks of FREEBUSY values.
func coalesce(periods []busyPeriod) []busyPeriod {
	sort.Slice(periods, func(i, j int) bool {
		if periods[i].fbType != periods[j].fbType {
			return periods[i].fbType < periods[j].fbType
		}
		return periods[i].start.Before(periods[j].start)
	})
	var merged []busyPeriod
	for _, p := range periods {
		last := len(merged) - 1
		if last >= 0 && merged[last].fbType == p.fbType && !p.start.After(merged[last].end) {
			if p.end.After(merged[last].end) {
				merged[last].end = p.end
			}
			continue
		}
		merged = append(merged, p)
	}
	return merged
}

func freeBusyCalendar(start, end time.Time, periods []busyPeriod) (*ical.Calendar, error) {
	var uid [16]byte
	if _, err := rand.Read(uid[:]); err != nil {
		return nil, fmt.Errorf("caldav: generating free-busy UID: %w", err)
	}

	fb := ical.NewComponent(ical.CompFreeBusy)
	fb.Props.SetText(ical.PropUID, hex.EncodeToString(uid[:]))
	fb.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	fb.Props.SetDateTime(ical.PropDateTimeStart, start)
	fb.Props.SetDateTime(ical.PropDateTimeEnd, end)
	for _, p := range periods {
		prop := ical.NewProp(ical.PropFreeBusy)
		prop.SetValueType(ical.ValuePeriod)
		prop.Value = p.start.Format(dateWithUTCTimeLayout) + "/" + p.end.Format(dateWithUTCTimeLayout)
		if p.fbType != "" {
			prop.Params.Set("FBTYPE", p.fbType)
		}
		fb.Props.Add(prop)
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//go-webdav//caldav//EN")
	cal.Children = append(cal.Children, fb)
	return cal, nil
}
