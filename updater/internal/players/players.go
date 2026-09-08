// Package players combines independent, fail-closed player observations.
package players

import (
	"regexp"
	"strconv"
	"strings"
)

type State string

const (
	Zero    State = "ZERO"
	NonZero State = "NON_ZERO"
	Unknown State = "UNKNOWN"
)

type Observation struct {
	Count int
	Valid bool
}

func Decide(a, b Observation) State {
	if a.Valid && b.Valid {
		if a.Count != b.Count {
			return Unknown
		}
		if a.Count == 0 {
			return Zero
		}
		return NonZero
	}
	if (a.Valid && a.Count > 0) || (b.Valid && b.Count > 0) {
		return NonZero
	}
	return Unknown
}

var rcon = regexp.MustCompile(`^There are ([0-9]+) of a max of ([0-9]+) players online:\s*(.*)$`)
var monitor = regexp.MustCompile(`^[^\s]+ : version=.+ online=([0-9]+) max=([0-9]+) motd='.*'$`)

func RCON(text string) Observation {
	m := rcon.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Observation{}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return Observation{}
	}
	max, err := strconv.Atoi(m[2])
	if err != nil || n > max {
		return Observation{}
	}
	names := strings.TrimSpace(m[3])
	if n == 0 && names != "" {
		return Observation{}
	}
	if n > 0 && (names == "" || len(strings.Split(names, ", ")) != n) {
		return Observation{}
	}
	return Observation{n, true}
}

func Monitor(text string) Observation {
	m := monitor.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Observation{}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return Observation{}
	}
	max, err := strconv.Atoi(m[2])
	if err != nil || max <= 0 || n > max {
		return Observation{}
	}
	return Observation{n, true}
}
