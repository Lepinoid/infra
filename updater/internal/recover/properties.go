package recover

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/lepinoid/infra/updater/internal/fsutil"
)

var ErrCAS = errors.New("persisted-whitelist-cas-conflict")
var property = regexp.MustCompile(`^\s*white-list\s*[=:]\s*(true|false)\s*$`)

func Whitelist(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	enabled, _, err := parseProperty(string(data))
	return enabled, err
}

func parseProperty(data string) (bool, int, error) {
	index := -1
	enabled := false
	for i, line := range strings.Split(data, "\n") {
		if m := property.FindStringSubmatch(line); m != nil {
			if index >= 0 {
				return false, 0, ErrCAS
			}
			index = i
			enabled = m[1] == "true"
		} else if strings.HasPrefix(strings.TrimSpace(line), "white-list") {
			return false, 0, ErrCAS
		}
	}
	if index < 0 {
		return false, 0, ErrCAS
	}
	return enabled, index, nil
}

func SetWhitelist(path string, expected, want bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	current, index, err := parseProperty(string(data))
	if err != nil {
		return err
	}
	if current != expected {
		return ErrCAS
	}
	lines := strings.Split(string(data), "\n")
	lines[index] = "white-list=" + strconv.FormatBool(want)
	return fsutil.Write(path, []byte(strings.Join(lines, "\n")))
}
