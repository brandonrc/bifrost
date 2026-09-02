package req

import (
	"fmt"
	"regexp"
	"strconv"
)

// Line is one REQ log line. It is the whole contract between tests and
// reqreport: tests emit it through t.Log, `go test -json` carries it
// verbatim in "output" events, reqreport parses it. Nothing else is shared.
//
// Format:  REQ: kind=<covers|notyetbuilt|skip> req=<n> reason=<quoted> [outcome=<failed|passed>]
type Line struct {
	Kind    string
	Req     int
	Reason  string
	Outcome string
}

const linePrefix = "REQ: "

// go's t.Log/t.Logf decorate each line with the caller's "file:line: "
// before the message when running under `go test` (with or without -v);
// tolerate that optional decoration so ParseLine can read real test output,
// not just Line.Format's own bare output.
var lineRe = regexp.MustCompile(`^\s*(?:[\w./\\-]+\.go:\d+: )?REQ: kind=(\w+) req=(\d+) reason=("(?:[^"\\]|\\.)*")(?: outcome=(\w+))?\s*$`)

// Format renders the line. Reason is %q-quoted so it may contain spaces.
func (l Line) Format() string {
	s := fmt.Sprintf("%skind=%s req=%d reason=%q", linePrefix, l.Kind, l.Req, l.Reason)
	if l.Outcome != "" {
		s += " outcome=" + l.Outcome
	}
	return s
}

// ParseLine parses one log line; ok=false for anything that is not a REQ line.
func ParseLine(s string) (Line, bool) {
	m := lineRe.FindStringSubmatch(s)
	if m == nil {
		return Line{}, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return Line{}, false
	}
	reason, err := strconv.Unquote(m[3])
	if err != nil {
		return Line{}, false
	}
	return Line{Kind: m[1], Req: n, Reason: reason, Outcome: m[4]}, true
}
