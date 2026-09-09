package caldav

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// The generated definition spans a fixed window so that it is a pure function
// of the zone name and the tzdata in the binary. Deriving either bound from
// the current date would change the bytes as time passed, which churns the
// ETag of every object carrying the definition and breaks feed merging, where
// the first definition seen for a TZID wins and all others are discarded.
const (
	vtzAnchorYear  = 1970
	vtzHorizonYear = 2075
	// vtzOngoingYears is how close to the end of the probe an era must still be
	// running to count as the rule in force. The probe stops mid-year, so the
	// final changeover of a live rule can fall a year short.
	vtzOngoingYears = 2
	// vtzTailYears is the closing stretch of the probe that an unbounded rule
	// has to account for completely before it may be published.
	vtzTailYears = 12
	// vtzRuleOnsets is how many changeovers an era needs before it may be
	// called a standing rule. Zones that change on "the Sunday on or after the
	// 2nd" land on a different ordinal weekday in some years, which breaks the
	// era in two; without this a stray one-year fragment sitting at the end of
	// the probe would be published as a yearly rule that never happens.
	vtzRuleOnsets = 3
)

// vtzFloatingLayout is RFC 5545 local time: no zone suffix. An observance's
// onsets are wall clocks in the offset it moves from, not instants.
const vtzFloatingLayout = "20060102T150405"

// VTimezone builds a VTIMEZONE component describing an IANA zone, suitable for
// embedding beside events that reference it by TZID.
//
// A reference to a TZID is only meaningful if the reader can resolve it, and
// Go cannot build a transition-aware location from anything but its own
// zoneinfo, so an object crossing a wire needs the definition travelling with
// it. This produces one observance per distinct changeover rule, each carrying
// the rule as an RRULE, with the rule currently in force left open ended so a
// client expanding a recurring event into the future still applies the right
// offset.
func VTimezone(name string) (*ical.Component, error) {
	// Both of these load successfully in Go. The empty name yields UTC under an
	// empty TZID, which is invalid and which this package's own parser rejects;
	// "Local" yields whatever the generating machine is set to, so the output
	// would stop being a function of its input.
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("caldav: %q is not an interchangeable time zone identifier", name)
	}

	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("caldav: unknown time zone %q: %w", name, err)
	}

	component := ical.NewComponent(ical.CompTimezone)
	component.Props.SetText(ical.PropTimezoneID, name)

	eras := groupTransitions(zoneTransitions(location, vtzAnchorYear, vtzHorizonYear))
	if len(eras) == 0 {
		component.Children = append(component.Children, fixedOffsetObservance(location))
		return component, nil
	}

	for i := range eras {
		component.Children = append(component.Children, eras[i].component())
	}

	return component, nil
}

// zoneTransition is one changeover: the instant it happens, the offsets either
// side of it, and whether the offset it moves to is a daylight one.
type zoneTransition struct {
	at         time.Time
	offsetFrom time.Duration
	offsetTo   time.Duration
	name       string
	isDST      bool
}

// onset is the changeover as an observance states it: a wall clock read in the
// offset being moved from.
func (t zoneTransition) onset() time.Time {
	return t.at.Add(t.offsetFrom).UTC()
}

// zoneTransitions finds every changeover in the window by walking a day at a
// time and bisecting the day the offset changes. Go keeps its transition table
// unexported, so probing is the only way to read it.
func zoneTransitions(location *time.Location, fromYear, toYear int) []zoneTransition {
	cursor := time.Date(fromYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(toYear, time.January, 1, 0, 0, 0, 0, time.UTC)

	_, previousOffset := cursor.In(location).Zone()

	var transitions []zoneTransition
	for cursor.Before(end) {
		probe := cursor.AddDate(0, 0, 1)
		if probe.After(end) {
			probe = end
		}

		if _, offset := probe.In(location).Zone(); offset == previousOffset {
			cursor = probe
			continue
		}

		// The offset is read at the changeover itself, not at the end of the
		// probe window, and the walk resumes from there. A day holding two
		// changeovers — a base offset moving on the same day as a daylight
		// one, which around thirty zones do — would otherwise be fused into a
		// single transition carrying the first one's timing and the second
		// one's offset.
		at := bisectTransition(location, cursor, probe, previousOffset)
		local := at.In(location)
		name, offset := local.Zone()

		transitions = append(transitions, zoneTransition{
			at:         at,
			offsetFrom: time.Duration(previousOffset) * time.Second,
			offsetTo:   time.Duration(offset) * time.Second,
			name:       name,
			isDST:      local.IsDST(),
		})
		previousOffset = offset
		cursor = at
	}

	return transitions
}

// bisectTransition narrows to the second at which the offset stops being the
// one in force before it.
//
// The result is truncated to a whole second. Halving a duration works in
// nanoseconds, so the search otherwise returns an instant carrying a fraction
// that no zone database expresses and that every consumer here formats away —
// which makes it invisible in output and quietly poisonous to any caller that
// compares instants.
func bisectTransition(location *time.Location, before, after time.Time, offsetBefore int) time.Time {
	for after.Sub(before) > time.Second {
		middle := before.Add(after.Sub(before) / 2)
		if _, offset := middle.In(location).Zone(); offset == offsetBefore {
			before = middle
			continue
		}
		after = middle
	}

	return after.Truncate(time.Second)
}

// observanceEra is a run of changeovers that all follow one rule, so they
// collapse into a single sub-component with a yearly recurrence.
type observanceEra struct {
	isDST      bool
	offsetFrom time.Duration
	offsetTo   time.Duration
	name       string
	month      time.Month
	weekday    time.Weekday
	// minDay and maxDay bound the days of the month the onsets fall on. A rule
	// fixed to an ordinal keeps them inside one seven-day step; a rule of the
	// form "the Friday on or after the 23rd" moves across a seven-day window.
	minDay  int
	maxDay  int
	allLast bool // every onset is the last such weekday of its month
	onsets  []time.Time
	ongoing bool
}

// ruleWindowDays is the span a single yearly weekday rule can describe: one
// weekday falls in any seven consecutive days exactly once.
const ruleWindowDays = 7

// canAbsorb reports whether a changeover continues this era. The two must
// agree on everything a rule states, and the onset must keep the era inside a
// single seven-day window, since that is the widest span one weekday rule can
// name without also matching a second day.
func (e *observanceEra) canAbsorb(other *observanceEra) bool {
	first, second := e.onsets[0], other.onsets[0]
	if e.isDST != other.isDST ||
		e.offsetFrom != other.offsetFrom ||
		e.offsetTo != other.offsetTo ||
		e.month != other.month ||
		e.weekday != other.weekday ||
		first.Hour() != second.Hour() ||
		first.Minute() != second.Minute() ||
		first.Second() != second.Second() {
		return false
	}

	low, high := min(e.minDay, other.minDay), max(e.maxDay, other.maxDay)

	return high-low < ruleWindowDays
}

func (e *observanceEra) absorb(other *observanceEra) {
	e.onsets = append(e.onsets, other.onsets...)
	e.minDay = min(e.minDay, other.minDay)
	e.maxDay = max(e.maxDay, other.maxDay)
	e.allLast = e.allLast && other.allLast
}

// byDay renders the era's position in the month.
//
// An ordinal is used when every onset shares one, which is the conventional
// and compact form. Where it does not — a rule like "the Friday on or after
// the 23rd", which Israel, Chile and Greenland all use — the ordinal shifts
// between years, and pinning one would put the changeover a week out in the
// years it does not fit. RFC 5545 intersects BYDAY with BYMONTHDAY, so naming
// the weekday and the seven days it may fall on says exactly the right thing.
func (e *observanceEra) byDay() string {
	names := [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
	weekday := names[e.weekday]

	if e.allLast {
		return "-1" + weekday
	}
	if (e.minDay-1)/ruleWindowDays == (e.maxDay-1)/ruleWindowDays {
		return fmt.Sprintf("%d%s", (e.minDay-1)/ruleWindowDays+1, weekday)
	}

	days := make([]string, 0, ruleWindowDays)
	for day := e.minDay; day < e.minDay+ruleWindowDays; day++ {
		days = append(days, strconv.Itoa(day))
	}

	return weekday + ";BYMONTHDAY=" + strings.Join(days, ",")
}

// groupTransitions collapses changeovers into eras. Two changeovers belong
// together when they say the same thing — same offsets, same position in the
// month, same time of day — in consecutive years.
func groupTransitions(transitions []zoneTransition) []observanceEra {
	var eras []observanceEra

	for i := range transitions {
		candidate := eraOf(transitions[i])
		if last := findOpenEra(eras, &candidate); last >= 0 {
			eras[last].absorb(&candidate)
			continue
		}
		eras = append(eras, candidate)
	}

	markOngoing(eras, transitions)

	sort.SliceStable(eras, func(a, b int) bool {
		return eras[a].onsets[0].Before(eras[b].onsets[0])
	})

	return eras
}

// markOngoing decides which eras may be published without a bound, meaning
// they claim to repeat forever. An era qualifies only if it reaches the end of
// the probe and has repeated often enough to be a rule rather than a one-off
// fragment that happened to land near the end.
//
// A published tail is exact for a zone that changes on a fixed annual rule,
// which is nearly all of them. It is an approximation for a zone whose
// changeovers follow something a yearly recurrence cannot express, such as the
// Ramadan adjustments in Asia/Gaza. Publishing an approximation beats
// publishing nothing, which would leave a reader stuck on the last changeover
// for every year after the window. changeoversInFinalYear reports whether the
// tail is complete, so a caller that cares can tell the two apart.
func markOngoing(eras []observanceEra, transitions []zoneTransition) {
	lastByDirection := map[bool]time.Time{}
	for i := range eras {
		last := eras[i].onsets[len(eras[i].onsets)-1]
		if last.After(lastByDirection[eras[i].isDST]) {
			lastByDirection[eras[i].isDST] = last
		}
	}

	for i := range eras {
		onsets := eras[i].onsets
		// Holding the final changeover of its direction is what makes an era
		// the one still in force. Anything short of that has been superseded,
		// and publishing it unbounded would keep generating onsets over the
		// top of whatever replaced it: America/Santiago moves on the Sunday on
		// or after the 2nd, so its ordinal weekday shifts between years, and
		// an unbounded rule from the earlier run puts the changeover a week
		// early in the years the later run actually covers.
		last := onsets[len(onsets)-1]
		holdsLast := last.Equal(lastByDirection[eras[i].isDST])
		// Holding the last changeover of its own direction is not enough on
		// its own: a zone that stopped changing altogether still has a final
		// daylight onset, and Asia/Ust-Nera's ended in 2010. The era must also
		// run to the end of the probe to be the rule still in force.
		reachesEnd := last.Year() >= vtzHorizonYear-vtzOngoingYears
		eras[i].ongoing = holdsLast && reachesEnd &&
			len(onsets) >= vtzRuleOnsets &&
			explainsRecentYears(&eras[i], transitions)
	}
}

// explainsRecentYears reports whether an era accounts for every changeover of
// its own direction in the closing stretch of the probe.
//
// A rule published without a bound keeps generating onsets forever, so it has
// to be the only thing happening in its direction, not merely the most common.
// Africa/Cairo ends summer time on the last Thursday of October at midnight,
// which puts the onset on the Friday after, and in some years that Friday
// falls in November. Those years form eras of their own, so an unbounded
// October rule would go on asserting an October changeover in years the zone
// actually changes in November, leaving a week of readings an hour out.
func explainsRecentYears(era *observanceEra, transitions []zoneTransition) bool {
	mine := make(map[time.Time]bool, len(era.onsets))
	for _, onset := range era.onsets {
		mine[onset] = true
	}

	for i := range transitions {
		if transitions[i].isDST != era.isDST {
			continue
		}
		onset := transitions[i].onset()
		if onset.Year() < vtzHorizonYear-vtzTailYears {
			continue
		}
		if !mine[onset] {
			return false
		}
	}

	return true
}

// changeoversInFinalYear counts the changeovers in the last year the probe
// covers in full. A published tail is complete, and therefore exact for every
// later year, only when it has one open-ended rule per changeover counted here.
func changeoversInFinalYear(transitions []zoneTransition) int {
	finalYear := vtzHorizonYear - 1

	count := 0
	for i := range transitions {
		if transitions[i].at.Year() == finalYear {
			count++
		}
	}

	return count
}

// findOpenEra returns the era the changeover continues, or -1 when it starts a
// new one. An era continues only into the very next year: a gap means the rule
// lapsed and resumed, which is two eras, not one with a hole.
func findOpenEra(eras []observanceEra, candidate *observanceEra) int {
	for i := len(eras) - 1; i >= 0; i-- {
		if !eras[i].canAbsorb(candidate) {
			continue
		}
		last := eras[i].onsets[len(eras[i].onsets)-1]
		if candidate.onsets[0].Year() != last.Year()+1 {
			return -1
		}

		return i
	}

	return -1
}

func eraOf(transition zoneTransition) observanceEra {
	onset := transition.onset()

	return observanceEra{
		isDST:      transition.isDST,
		offsetFrom: transition.offsetFrom,
		offsetTo:   transition.offsetTo,
		name:       transition.name,
		month:      onset.Month(),
		weekday:    onset.Weekday(),
		minDay:     onset.Day(),
		maxDay:     onset.Day(),
		allLast:    isLastWeekdayOfMonth(onset),
		onsets:     []time.Time{onset},
	}
}

func isLastWeekdayOfMonth(day time.Time) bool {
	daysInMonth := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()

	return day.Day()+ruleWindowDays > daysInMonth
}

func (e *observanceEra) component() *ical.Component {
	kind := ical.CompTimezoneStandard
	if e.isDST {
		kind = ical.CompTimezoneDaylight
	}

	observance := ical.NewComponent(kind)
	setFloatingStart(observance, e.onsets[0])
	observance.Props.Set(rawProp("TZOFFSETFROM", formatUTCOffset(offsetSeconds(e.offsetFrom))))
	observance.Props.Set(rawProp("TZOFFSETTO", formatUTCOffset(offsetSeconds(e.offsetTo))))
	if e.name != "" {
		observance.Props.SetText(ical.PropTimezoneName, e.name)
	}
	if rule := e.recurrenceRule(); rule != "" {
		observance.Props.Set(rawProp(ical.PropRecurrenceRule, rule))
	}

	return observance
}

// recurrenceRule terminates a finished era the way RFC 5545 §3.6.5 requires:
// with UNTIL naming the last onset as a UTC instant, which is the onset's wall
// clock less the offset being left.
func (e *observanceEra) recurrenceRule() string {
	if len(e.onsets) == 1 && !e.ongoing {
		return ""
	}

	parts := []string{
		"FREQ=YEARLY",
		fmt.Sprintf("BYMONTH=%d", int(e.month)),
		"BYDAY=" + e.byDay(),
	}
	if !e.ongoing {
		final := e.onsets[len(e.onsets)-1].Add(-e.offsetFrom)
		parts = append(parts, "UNTIL="+final.Format(localDateTimeLayout+"Z"))
	}

	return strings.Join(parts, ";")
}

// fixedOffsetObservance covers a zone that never changes offset in the window.
// It carries no recurrence, because an RRULE here would assert a changeover
// that does not happen.
func fixedOffsetObservance(location *time.Location) *ical.Component {
	anchor := time.Date(vtzAnchorYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	name, seconds := anchor.In(location).Zone()
	offset := time.Duration(seconds) * time.Second

	// The onset is the anchor itself, not the anchor shifted into local time.
	// Both offsets are the same here, so the instant carries no information,
	// and shifting it puts the wall clock in the year before the window for
	// every zone west of Greenwich.
	observance := ical.NewComponent(ical.CompTimezoneStandard)
	setFloatingStart(observance, anchor)
	observance.Props.Set(rawProp("TZOFFSETFROM", formatUTCOffset(offsetSeconds(offset))))
	observance.Props.Set(rawProp("TZOFFSETTO", formatUTCOffset(offsetSeconds(offset))))
	if name != "" {
		observance.Props.SetText(ical.PropTimezoneName, name)
	}

	return observance
}

func setFloatingStart(observance *ical.Component, onset time.Time) {
	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.SetValueType(ical.ValueDateTime)
	prop.Value = onset.Format(vtzFloatingLayout)
	observance.Props.Set(prop)
}

// offsetSeconds adapts a duration to the package's existing offset formatter.
func offsetSeconds(offset time.Duration) int {
	return int(offset / time.Second)
}
