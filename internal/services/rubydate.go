package services

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	monthNames   = "jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec"
	dayNames     = "sun|mon|tue|wed|thu|fri|sat"
	eraNames     = `c(?:e|\.e\.)|b(?:ce|\.c\.e\.)|a(?:d|\.d\.)|b(?:c|\.c\.)`
	maxDateInput = 128
	maxClockHour = 24
	secondsMax   = 59
	leapSecond   = 60
)

func ci(p string) *regexp.Regexp { return regexp.MustCompile("(?i)" + p) }

var (
	dateNoise = regexp.MustCompile(`[^-+',./:@[:alnum:]\[\]]+`)

	dayRx  = ci(`\b(` + dayNames + `)[^-/\d\s]*`)
	timeRx = ci(`((?:\d+\s*:\s*\d+(?:\s*:\s*\d+(?:[,.]\d*)?)?|\d+\s*h(?:\s*\d+m?(?:\s*\d+s?)?)?)(?:\s*[ap](?:m\b|\.m\.))?|\d+\s*[ap](?:m\b|\.m\.))` +
		`(?:\s*((?:gmt|utc?)?[-+]\d+(?:[,.:]\d+(?::\d+)?)?|[[:alpha:].\s]+(?:standard|daylight)\stime\b|[[:alpha:]]+(?:\sdst)?\b))?`)
	timePartsRx = ci(`\A(\d+)h?(?:\s*:?\s*(\d+)m?(?:\s*:?\s*(\d+)(?:[,.](\d+))?s?)?)?(?:\s*([ap])(?:m\b|\.m\.))?`)
	euRx        = ci(`('?\d+)[^-\d\s]*\s*(` + monthNames + `)[^-\d\s']*(?:\s*(` + eraNames + `)?\s*('?-?\d+(?:(?:st|nd|rd|th)\b)?))?`)
	usRx        = ci(`\b(` + monthNames + `)[^-\d\s']*\s*('?\d+)[^-\d\s']*(?:\s*,?\s*(` + eraNames + `)?\s*('?-?\d+))?`)
	isoRx       = ci(`('?[-+]?\d+)-(\d+)-('?-?\d+)`)
	jisRx       = ci(`\b([mtshr])(\d+)\.(\d+)\.(\d+)`)
	vms11Rx     = ci(`('?-?\d+)-(` + monthNames + `)[^-/.]*-('?-?\d+)`)
	vms12Rx     = ci(`\b(` + monthNames + `)[^-/.]*-('?-?\d+)(?:-('?-?\d+))?`)
	slaRx       = ci(`('?-?\d+)/\s*('?\d+)(?:\D\s*('?-?\d+))?`)
	dotRx       = ci(`('?-?\d+)\.\s*('?\d+)\.\s*('?-?\d+)`)
	iso21Rx     = ci(`\b(\d{2}|\d{4})?-?w(\d{2})(?:-?(\d))?\b`)
	iso22Rx     = ci(`-w-(\d)\b`)
	iso23Rx     = ci(`--(\d{2})?-(\d{2})\b`)
	iso24Rx     = ci(`--(\d{2})(\d{2})?\b`)
	iso25Guard  = ci(`[,.](\d{2}|\d{4})-\d{3}\b`)
	iso25Rx     = ci(`\b(\d{2}|\d{4})-(\d{3})\b`)
	iso26Guard  = ci(`\d-\d{3}\b`)
	iso26Rx     = ci(`\b-(\d{3})\b`)
	yearRx      = ci(`'(\d+)\b`)
	monRx       = ci(`\b(` + monthNames + `)\S*`)
	mdayRx      = ci(`(\d+)(st|nd|rd|th)\b`)
	dddRx       = ci(`([-+]?)(\d{2,14})(?:\s*t?\s*(\d{2,6})?(?:[,.](\d*))?)?(?:\s*(z\b|[-+]\d{1,4}\b|\[[-+]?\d[^\]]*\]))?`)
	bcRx        = ci(`\b(bc\b|bce\b|b\.c\.|b\.c\.e\.)`)
	fragRx      = ci(`\A\s*(\d{1,2})\s*\z`)
	leadingNum  = regexp.MustCompile(`\d+`)
	signedNum   = regexp.MustCompile(`([-+])?(\d+)`)
	yearPartRx  = regexp.MustCompile(`(\d+)(.+)?`)
)

type frags struct {
	v       map[string]int
	comp    bool
	bc      bool
	yearBig *big.Int
	nth     *big.Int
}

const (
	bigYearDigits = 12
	// date_core.c's CM_PERIOD days and the Julian years it spans.
	cmPeriod      = 213447717
	cmPeriodYears = 584400
)

func (f *frags) setYear(sign int, digits string) {
	if len(strings.TrimLeft(digits, "0")) <= bigYearDigits {
		f.v["year"] = sign * atoi(digits)
		return
	}
	n, _ := new(big.Int).SetString(digits, 10)
	if sign < 0 {
		n.Neg(n)
	}
	f.yearBig, f.v["year"] = n, 0
}

func (f *frags) reduceYear() {
	year, ok := f.v["year"]
	if !ok {
		return
	}
	y := big.NewInt(int64(year))
	if f.yearBig != nil {
		y = f.yearBig
		if f.bc {
			y = new(big.Int).Sub(big.NewInt(1), y)
		}
	}
	t := new(big.Int).Add(y, big.NewInt(julianEpochOff))
	period := big.NewInt(cmPeriodYears)
	nth, rem := new(big.Int).DivMod(t, period, new(big.Int))
	f.nth = nth
	f.v["year"] = int(rem.Int64()) - julianEpochOff
}

func (f *frags) has(k string) bool { _, ok := f.v[k]; return ok }

type match []*string

func (m match) at(i int) (string, bool) {
	if m[i] == nil {
		return "", false
	}
	return *m[i], true
}

func (m match) str(i int) string { s, _ := m.at(i); return s }

// subx removes the first match of re from s, as Date._parse does, and hands it to cb.
func subx(s *string, re *regexp.Regexp, cb func(m match)) bool {
	loc := re.FindStringSubmatchIndex(*s)
	if loc == nil {
		return false
	}
	m := make(match, len(loc)/2)
	for i := range m {
		if loc[2*i] >= 0 {
			g := (*s)[loc[2*i]:loc[2*i+1]]
			m[i] = &g
		}
	}
	*s = (*s)[:loc[0]] + " " + (*s)[loc[1]:]
	cb(m)
	return true
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func monthNumber(abbr string) int {
	return strings.Index(monthNames, strings.ToLower(abbr[:3]))/4 + 1
}

func (f *frags) s3e(y, m, d *string, bc bool) {
	val := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	if y != nil && m != nil && d == nil {
		y, m, d = d, y, m
	}
	if y == nil {
		if d != nil && len(*d) > 2 {
			y, d = d, nil
		}
		if d != nil && strings.HasPrefix(*d, "'") {
			y, d = d, nil
		}
	}
	if y != nil {
		if g := yearPartRx.FindStringSubmatch(*y); g != nil && g[2] != "" {
			trail := g[1]
			y, d = d, &trail
		}
	}
	if m != nil && (strings.HasPrefix(*m, "'") || len(*m) > 2) {
		y, m, d = m, d, y
	}
	if d != nil && (strings.HasPrefix(*d, "'") || len(*d) > 2) {
		y, d = d, y
	}
	if y != nil {
		if g := signedNum.FindStringSubmatch(*y); g != nil {
			if g[1] != "" || len(g[2]) > 2 {
				f.comp = false
			}
			sign := 1
			if g[1] == "-" {
				sign = -1
			}
			f.setYear(sign, g[2])
		}
	}
	if bc {
		f.bc = true
	}
	if m != nil {
		if g := leadingNum.FindString(val(m)); g != "" {
			f.v["mon"] = atoi(g)
		}
	}
	if d != nil {
		if g := leadingNum.FindString(val(d)); g != "" {
			f.v["mday"] = atoi(g)
		}
	}
}

func isBC(era string) bool { return era != "" && (era[0] == 'b' || era[0] == 'B') }

func ptrIf(m match, i int) *string { return m[i] }

func rubyDateParse(input string) (*frags, error) {
	if len(input) > maxDateInput {
		return nil, errors.New("string length exceeds the limit")
	}
	f := &frags{v: map[string]int{}, comp: true}
	s := dateNoise.ReplaceAllString(input, " ")

	subx(&s, dayRx, func(m match) { f.v["wday"] = strings.Index(dayNames, strings.ToLower(m.str(1))) / 4 })
	subx(&s, timeRx, func(m match) {
		g := timePartsRx.FindStringSubmatch(m.str(1))
		if g == nil {
			return
		}
		hour := atoi(g[1])
		if g[5] != "" {
			hour %= 12
			if strings.EqualFold(g[5], "p") {
				hour += 12
			}
		}
		f.v["hour"] = hour
		if g[2] != "" {
			f.v["min"] = atoi(g[2])
		}
		if g[3] != "" {
			f.v["sec"] = atoi(g[3])
		}
	})

	parsed := subx(&s, euRx, func(m match) {
		f.s3e(ptrIf(m, 4), monthStr(m.str(2)), ptrIf(m, 1), isBC(m.str(3)))
	}) ||
		subx(&s, usRx, func(m match) {
			f.s3e(ptrIf(m, 4), monthStr(m.str(1)), ptrIf(m, 2), isBC(m.str(3)))
		}) ||
		subx(&s, isoRx, func(m match) { f.s3e(ptrIf(m, 1), ptrIf(m, 2), ptrIf(m, 3), false) }) ||
		subx(&s, jisRx, func(m match) {
			base := map[string]int{"m": 1867, "t": 1911, "s": 1925, "h": 1988, "r": 2018}[strings.ToLower(m.str(1))]
			f.v["year"], f.v["mon"], f.v["mday"] = atoi(m.str(2))+base, atoi(m.str(3)), atoi(m.str(4))
		}) ||
		subx(&s, vms11Rx, func(m match) { f.s3e(ptrIf(m, 3), monthStr(m.str(2)), ptrIf(m, 1), false) }) ||
		subx(&s, vms12Rx, func(m match) { f.s3e(ptrIf(m, 3), monthStr(m.str(1)), ptrIf(m, 2), false) }) ||
		subx(&s, slaRx, func(m match) { f.s3e(ptrIf(m, 1), ptrIf(m, 2), ptrIf(m, 3), false) }) ||
		subx(&s, dotRx, func(m match) { f.s3e(ptrIf(m, 1), ptrIf(m, 2), ptrIf(m, 3), false) }) ||
		f.parseISO2(&s) ||
		subx(&s, yearRx, func(m match) { f.setYear(1, m.str(1)) }) ||
		subx(&s, monRx, func(m match) { f.v["mon"] = monthNumber(m.str(1)) }) ||
		subx(&s, mdayRx, func(m match) { f.v["mday"] = atoi(m.str(1)) }) ||
		subx(&s, dddRx, f.ddd)
	_ = parsed

	subx(&s, bcRx, func(match) { f.bc = true })
	subx(&s, fragRx, func(m match) {
		n := atoi(m.str(1))
		switch {
		case f.has("hour") && !f.has("mday"):
			if n >= 1 && n <= 31 {
				f.v["mday"] = n
			}
		case f.has("mday") && !f.has("hour"):
			if n >= 0 && n <= maxClockHour {
				f.v["hour"] = n
			}
		}
	})

	if f.bc {
		for _, k := range []string{"cwyear", "year"} {
			if y, ok := f.v[k]; ok {
				f.v[k] = -y + 1
			}
		}
	}
	if f.comp {
		for _, k := range []string{"cwyear", "year"} {
			if y, ok := f.v[k]; ok && y >= 0 && y <= 99 {
				if y >= 69 {
					f.v[k] = y + 1900
				} else {
					f.v[k] = y + 2000
				}
			}
		}
	}
	f.reduceYear()
	return f, nil
}

func monthStr(abbr string) *string {
	s := strconv.Itoa(monthNumber(abbr))
	return &s
}

func (f *frags) parseISO2(s *string) bool {
	if subx(s, iso21Rx, func(m match) {
		if y, ok := m.at(1); ok {
			f.v["cwyear"] = atoi(y)
		}
		f.v["cweek"] = atoi(m.str(2))
		if d, ok := m.at(3); ok {
			f.v["cwday"] = atoi(d)
		}
	}) {
		return true
	}
	if subx(s, iso22Rx, func(m match) { f.v["cwday"] = atoi(m.str(1)) }) {
		return true
	}
	if subx(s, iso23Rx, func(m match) {
		if mon, ok := m.at(1); ok {
			f.v["mon"] = atoi(mon)
		}
		f.v["mday"] = atoi(m.str(2))
	}) {
		return true
	}
	if subx(s, iso24Rx, func(m match) {
		f.v["mon"] = atoi(m.str(1))
		if d, ok := m.at(2); ok {
			f.v["mday"] = atoi(d)
		}
	}) {
		return true
	}
	if !iso25Guard.MatchString(*s) && subx(s, iso25Rx, func(m match) {
		f.v["year"], f.v["yday"] = atoi(m.str(1)), atoi(m.str(2))
	}) {
		return true
	}
	return !iso26Guard.MatchString(*s) && subx(s, iso26Rx, func(m match) { f.v["yday"] = atoi(m.str(1)) })
}

func tail(s string, n int) int { return atoi(s[len(s)-n:]) }

func (f *frags) ddd(m match) {
	sign := 1
	if m.str(1) == "-" {
		sign = -1
	}
	s2 := m.str(2)
	s3, has3 := m.at(3)
	_, has4 := m.at(4)
	clock := !has3 && has4
	l2 := len(s2)
	switch l2 {
	case 2:
		if clock {
			f.v["sec"] = tail(s2, 2)
		} else {
			f.v["mday"] = atoi(s2[:2])
		}
	case 4:
		if clock {
			f.v["sec"], f.v["min"] = tail(s2, 2), atoi(s2[l2-4:l2-2])
		} else {
			f.v["mon"], f.v["mday"] = atoi(s2[:2]), atoi(s2[2:4])
		}
	case 6:
		if clock {
			f.v["sec"], f.v["min"], f.v["hour"] = tail(s2, 2), atoi(s2[l2-4:l2-2]), atoi(s2[l2-6:l2-4])
		} else {
			f.v["year"], f.v["mon"], f.v["mday"] = sign*atoi(s2[:2]), atoi(s2[2:4]), atoi(s2[4:6])
		}
	case 8, 10, 12, 14:
		if clock {
			f.v["sec"], f.v["min"], f.v["hour"], f.v["mday"] = tail(s2, 2), atoi(s2[l2-4:l2-2]), atoi(s2[l2-6:l2-4]), atoi(s2[l2-8:l2-6])
			if l2 >= 10 {
				f.v["mon"] = atoi(s2[l2-10 : l2-8])
			}
			if l2 == 12 {
				f.v["year"] = sign * atoi(s2[l2-12:l2-10])
			}
			if l2 == 14 {
				f.v["year"] = sign * atoi(s2[l2-14:l2-10])
				f.comp = false
			}
		} else {
			f.v["year"], f.v["mon"], f.v["mday"] = sign*atoi(s2[:4]), atoi(s2[4:6]), atoi(s2[6:8])
			if l2 >= 10 {
				f.v["hour"] = atoi(s2[8:10])
			}
			if l2 >= 12 {
				f.v["min"] = atoi(s2[10:12])
			}
			if l2 >= 14 {
				f.v["sec"] = atoi(s2[12:14])
			}
			f.comp = false
		}
	case 3:
		if clock {
			f.v["sec"], f.v["min"] = tail(s2, 2), atoi(s2[l2-3:l2-2])
		} else {
			f.v["yday"] = atoi(s2)
		}
	case 5:
		if clock {
			f.v["sec"], f.v["min"], f.v["hour"] = tail(s2, 2), atoi(s2[l2-4:l2-2]), atoi(s2[l2-5:l2-4])
		} else {
			f.v["year"], f.v["yday"] = sign*atoi(s2[:2]), atoi(s2[2:5])
		}
	case 7:
		if clock {
			f.v["sec"], f.v["min"], f.v["hour"], f.v["mday"] = tail(s2, 2), atoi(s2[l2-4:l2-2]), atoi(s2[l2-6:l2-4]), atoi(s2[l2-7:l2-6])
		} else {
			f.v["year"], f.v["yday"] = sign*atoi(s2[:4]), atoi(s2[4:7])
		}
	}
	if has3 {
		l3 := len(s3)
		if l3 == 2 || l3 == 4 || l3 == 6 {
			if has4 {
				f.v["sec"] = tail(s3, 2)
				if l3 >= 4 {
					f.v["min"] = atoi(s3[l3-4 : l3-2])
				}
				if l3 >= 6 {
					f.v["hour"] = atoi(s3[l3-6 : l3-4])
				}
			} else {
				f.v["hour"] = atoi(s3[:2])
				if l3 >= 4 {
					f.v["min"] = atoi(s3[2:4])
				}
				if l3 >= 6 {
					f.v["sec"] = atoi(s3[4:6])
				}
			}
		}
	}
}

// rubyDateTime is DateTime.parse(s).strftime('%Y-%m-%dT%H:%M:%SZ'); today stands in for Date.today.
func rubyDateTime(input string, today time.Time) (string, error) {
	f, err := rubyDateParse(input)
	if err != nil {
		return "", err
	}
	jd, err := f.julianDay(today)
	if err != nil {
		return "", fmt.Errorf("invalid date %q", input)
	}
	hour, min, sec := f.v["hour"], f.v["min"], f.v["sec"]
	if hour < 0 || hour > maxClockHour || min < 0 || min > secondsMax || sec < 0 || sec > secondsMax || (hour == maxClockHour && (min > 0 || sec > 0)) {
		return "", fmt.Errorf("invalid date %q", input)
	}
	if hour == maxClockHour {
		hour = 0
		jd++
	}
	periods := big.NewInt(int64(floorDiv(jd, cmPeriod)))
	if f.nth != nil {
		periods.Add(periods, f.nth)
	}
	year, mon, mday := jdToCivil(floorMod(jd, cmPeriod))
	y := new(big.Int).Mul(periods, big.NewInt(cmPeriodYears))
	y.Add(y, big.NewInt(int64(year)))
	return fmt.Sprintf("%s-%02d-%02dT%02d:%02d:%02dZ", yearText(y), mon, mday, hour, min, sec), nil
}

func yearText(y *big.Int) string {
	if y.Sign() < 0 {
		return fmt.Sprintf("-%04s", new(big.Int).Neg(y).String())
	}
	return fmt.Sprintf("%04s", y.String())
}

var errNoDate = errors.New("no date")

func (f *frags) hasCivilDate() bool {
	return !f.has("yday") && f.has("year") && f.has("mon") && f.has("mday")
}

type fragPattern struct {
	kind string
	keys []string
}

var (
	clockKeys    = []string{"hour", "min", "sec"}
	fragPatterns = []fragPattern{
		{"time", clockKeys},
		{"ordinal", append([]string{"year", "yday"}, clockKeys...)},
		{"civil", append([]string{"year", "mon", "mday"}, clockKeys...)},
		{"commercial", append([]string{"cwyear", "cweek", "cwday"}, clockKeys...)},
		{"wday", append([]string{"wday"}, clockKeys...)},
		{"wnum0", append([]string{"year", "wnum0", "wday"}, clockKeys...)},
		{"wnum1", append([]string{"year", "wnum1", "wday"}, clockKeys...)},
		{"", append([]string{"cwyear", "cweek", "wday"}, clockKeys...)},
		{"", append([]string{"year", "wnum0", "cwday"}, clockKeys...)},
		{"", append([]string{"year", "wnum1", "cwday"}, clockKeys...)},
	}
)

// julianDay follows DateTime's new_by_frags: complete_frags, then valid_date_frags, then the clock clamps.
func (f *frags) julianDay(today time.Time) (int, error) {
	if f.hasCivilDate() {
		jd, ok := validCivil(f.v["year"], f.v["mon"], f.v["mday"])
		if !ok {
			return 0, errNoDate
		}
		if f.v["sec"] == leapSecond {
			f.v["sec"] = secondsMax
		}
		return jd, nil
	}
	f.complete(today)
	jd, ok := f.validDateFrags()
	if !ok {
		return 0, errNoDate
	}
	return jd, nil
}

func (f *frags) complete(today time.Time) {
	kind, keys, best := "", []string(nil), 0
	for _, p := range fragPatterns {
		n := 0
		for _, k := range p.keys {
			if f.has(k) {
				n++
			}
		}
		if n > best {
			kind, keys, best = p.kind, p.keys, n
		}
	}
	todayJD := civilToJD(today.Year(), int(today.Month()), today.Day())
	isoYear, isoWeek := today.ISOWeek()
	isoDay := int(today.Weekday())
	if isoDay == 0 {
		isoDay = 7
	}
	todayValues := map[string]int{"year": today.Year(), "mon": int(today.Month()), "mday": today.Day(),
		"cwyear": isoYear, "cweek": isoWeek, "cwday": isoDay}
	fillLeading := func() {
		for _, k := range keys {
			v, known := todayValues[k]
			if f.has(k) || !known {
				return
			}
			f.v[k] = v
		}
	}
	setDefault := func(k string, v int) {
		if !f.has(k) {
			f.v[k] = v
		}
	}
	if kind != "" && len(keys) > best {
		switch kind {
		case "ordinal":
			setDefault("year", today.Year())
			setDefault("yday", 1)
		case "civil":
			fillLeading()
			setDefault("mon", 1)
			setDefault("mday", 1)
		case "commercial":
			fillLeading()
			setDefault("cweek", 1)
			setDefault("cwday", 1)
		case "wday":
			f.v["jd"] = todayJD - floorMod(todayJD+1, daysPerWeek) + f.v["wday"]
		case "wnum0", "wnum1":
			fillLeading()
			setDefault(kind, 0)
			setDefault("wday", 0)
		}
	}
	if kind == "time" {
		setDefault("jd", todayJD)
	}
	if f.v["sec"] > secondsMax {
		f.v["sec"] = secondsMax
	}
}

func (f *frags) validDateFrags() (int, bool) {
	if f.has("jd") {
		return f.v["jd"], true
	}
	year, hasYear := f.v["year"]
	if yday, ok := f.v["yday"]; ok && hasYear {
		if jd, valid := validOrdinal(year, yday); valid {
			return jd, true
		}
	}
	if f.has("mday") && f.has("mon") && hasYear {
		if jd, valid := validCivil(year, f.v["mon"], f.v["mday"]); valid {
			return jd, true
		}
	}
	cwday, hasCwday := f.v["cwday"]
	if wday, ok := f.v["wday"]; !hasCwday && ok {
		cwday, hasCwday = wday, true
		if wday == 0 {
			cwday = daysPerWeek
		}
	}
	if hasCwday && f.has("cweek") && f.has("cwyear") {
		if jd, valid := validCommercial(f.v["cwyear"], f.v["cweek"], cwday); valid {
			return jd, true
		}
	}
	weekday, hasWeekday := f.v["wday"]
	if !hasWeekday {
		if w, ok := f.v["cwday"]; ok {
			weekday, hasWeekday = w, true
		}
	}
	if hasWeekday && hasYear && f.has("wnum0") {
		sunday := weekday
		if !f.has("wday") && sunday == daysPerWeek {
			sunday = 0
		}
		if jd, valid := validWeekNumber(year, f.v["wnum0"], sunday, 0); valid {
			return jd, true
		}
	}
	if hasWeekday && hasYear && f.has("wnum1") {
		if jd, valid := validWeekNumber(year, f.v["wnum1"], floorMod(weekday-1, daysPerWeek), 1); valid {
			return jd, true
		}
	}
	return 0, false
}

const (
	daysPerWeek    = 7
	reformJD       = 2299161
	julianEpochOff = 4716
	julianYearLen  = 365.25
	monthLenFactor = 30.6001
)

func floorDiv(a, b int) int { return int(math.Floor(float64(a) / float64(b))) }

func floorMod(a, b int) int { return a - b*floorDiv(a, b) }

// civilToJD and jdToCivil are date_core.c's c_civil_to_jd and c_jd_to_civil with the Italian reform.
func civilToJD(y, m, d int) int {
	if m <= 2 {
		y--
		m += 12
	}
	a := math.Floor(float64(y) / 100.0)
	b := 2 - a + math.Floor(a/4.0)
	jd := math.Floor(julianYearLen*float64(y+julianEpochOff)) + math.Floor(monthLenFactor*float64(m+1)) + float64(d) + b - 1524
	if jd < reformJD {
		jd -= b
	}
	return int(jd)
}

func jdToCivil(jd int) (y, m, d int) {
	a := float64(jd)
	if jd >= reformJD {
		x := math.Floor((a - 1867216.25) / 36524.25)
		a = a + 1 + x - math.Floor(x/4.0)
	}
	b := a + 1524
	c := math.Floor((b - 122.1) / julianYearLen)
	days := math.Floor(julianYearLen * c)
	e := math.Floor((b - days) / monthLenFactor)
	dom := b - days - math.Floor(monthLenFactor*e)
	if e <= 13 {
		return int(c) - julianEpochOff, int(e) - 1, int(dom)
	}
	return int(c) - julianEpochOff + 1, int(e) - 13, int(dom)
}

func validCivil(y, m, d int) (int, bool) {
	jd := civilToJD(y, m, d)
	ry, rm, rd := jdToCivil(jd)
	return jd, ry == y && rm == m && rd == d
}

func validOrdinal(y, yday int) (int, bool) {
	first := civilToJD(y, 1, 1)
	jd := first + yday - 1
	ry, _, _ := jdToCivil(jd)
	return jd, ry == y && jd-civilToJD(ry, 1, 1)+1 == yday
}

func commercialToJD(y, w, d int) int {
	r := civilToJD(y, 1, 1) + 3
	return (r - floorMod(r, daysPerWeek)) + daysPerWeek*(w-1) + (d - 1)
}

func jdToCommercial(jd int) (y, w, d int) {
	a, _, _ := jdToCivil(jd - 3)
	start := commercialToJD(a+1, 1, 1)
	y = a + 1
	if jd < start {
		start = commercialToJD(a, 1, 1)
		y = a
	}
	d = floorMod(jd+1, daysPerWeek)
	if d == 0 {
		d = daysPerWeek
	}
	return y, 1 + floorDiv(jd-start, daysPerWeek), d
}

func validCommercial(y, w, d int) (int, bool) {
	jd := commercialToJD(y, w, d)
	ry, rw, rd := jdToCommercial(jd)
	return jd, ry == y && rw == w && rd == d
}

func weekNumberToJD(y, w, d, first int) int {
	r := civilToJD(y, 1, 1) + 6
	return (r - floorMod(r-first+1, daysPerWeek) - daysPerWeek) + daysPerWeek*w + d
}

func jdToWeekNumber(jd, first int) (y, w, d int) {
	y, _, _ = jdToCivil(jd)
	r := civilToJD(y, 1, 1) + 6
	j := jd - (r - floorMod(r-first+1, daysPerWeek)) + daysPerWeek
	return y, floorDiv(j, daysPerWeek), floorMod(j, daysPerWeek)
}

func validWeekNumber(y, w, d, first int) (int, bool) {
	jd := weekNumberToJD(y, w, d, first)
	ry, rw, rd := jdToWeekNumber(jd, first)
	return jd, ry == y && rw == w && rd == d
}
