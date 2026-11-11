package triage

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HoldFileName is the fleet-wide hold on new validation runs, under the
// no-mistakes home. Whoever needs the service quiet (an install of a new
// build) writes it; while it exists `axi triage` and `axi run` stop, and the
// daemon refuses to create a run. Runs already in flight are not touched.
const HoldFileName = "hold"

// Hold is a parsed hold file. Its format is three `key: value` lines - who
// holds new runs, why, and until when - but any content counts: a file that
// exists is a hold, and its raw text is shown when it does not parse.
type Hold struct {
	Path  string
	Who   string
	Why   string
	Until string
	Raw   string
}

// HoldPath returns the hold file under the no-mistakes home root.
func HoldPath(root string) string { return filepath.Join(root, HoldFileName) }

// ReadHold returns the active hold under root, or nil when there is none. A
// hold whose `until` is a timestamp already in the past has expired and is
// ignored, so a manual hold left behind cannot block the fleet forever; an
// `until` that is not a timestamp ("the install finishes") never expires.
// A hold file that exists but cannot be read is reported as a hold: when in
// doubt, no new run.
func ReadHold(root string, now time.Time) (*Hold, error) {
	path := HoldPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return &Hold{Path: path, Raw: fmt.Sprintf("unreadable hold file: %v", err)}, nil
	}
	h := &Hold{Path: path, Raw: strings.TrimSpace(string(data))}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "who":
			h.Who = value
		case "why":
			h.Why = value
		case "until":
			h.Until = value
		}
	}
	if until, ok := parseUntil(h.Until); ok && !now.Before(until) {
		return nil, nil
	}
	return h, nil
}

func parseUntil(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z", "2006-01-02 15:04Z07:00"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// String renders the hold for a STOP line.
func (h *Hold) String() string {
	if h == nil {
		return ""
	}
	if h.Who == "" && h.Why == "" && h.Until == "" {
		if h.Raw == "" {
			return "empty hold file " + h.Path
		}
		return h.Raw
	}
	parts := []string{}
	if h.Who != "" {
		parts = append(parts, "by "+h.Who)
	}
	if h.Why != "" {
		parts = append(parts, "why: "+h.Why)
	}
	if h.Until != "" {
		parts = append(parts, "until: "+h.Until)
	}
	return strings.Join(parts, "; ")
}

// HoldError is the daemon's refusal to create a run while a hold is active.
func HoldError(h *Hold) error {
	return fmt.Errorf("new validation runs are on hold (%s); runs already in flight continue - start this run after the hold file %s is removed", h.String(), h.Path)
}
