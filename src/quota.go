package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type windowKind uint8

const (
	otherWindow windowKind = iota
	fiveHour
	sevenDay
)

type quotaWindow struct {
	kind      windowKind
	scope     string
	remaining float64
	reset     time.Time
}

type quotaSnapshot struct {
	fetchedAt time.Time
	windows   []quotaWindow
}

type codexWindow struct {
	UsedPercent *float64        `json:"used_percent"`
	Seconds     float64         `json:"limit_window_seconds"`
	ResetAt     json.RawMessage `json:"reset_at"`
	ResetAfter  *float64        `json:"reset_after_seconds"`
}

type codexLimit struct {
	Allowed   *bool        `json:"allowed"`
	Reached   bool         `json:"limit_reached"`
	Primary   *codexWindow `json:"primary_window"`
	Secondary *codexWindow `json:"secondary_window"`
}

type claudeWindow struct {
	Utilization      *float64        `json:"utilization"`
	ResetAt          json.RawMessage `json:"resets_at"`
	LimitDollars     *float64        `json:"limit_dollars"`
	UsedDollars      *float64        `json:"used_dollars"`
	RemainingDollars *float64        `json:"remaining_dollars"`
}

func parseQuota(raw []byte, provider string, now time.Time) (quotaSnapshot, error) {
	q := quotaSnapshot{fetchedAt: now}
	switch provider {
	case "codex":
		var usage struct {
			RateLimit  *codexLimit `json:"rate_limit"`
			Additional []struct {
				Name      string      `json:"limit_name"`
				Feature   string      `json:"metered_feature"`
				RateLimit *codexLimit `json:"rate_limit"`
			} `json:"additional_rate_limits"`
		}
		if err := json.Unmarshal(raw, &usage); err != nil {
			return quotaSnapshot{}, fmt.Errorf("decode Codex usage")
		}
		q.addCodexLimit(usage.RateLimit, "", now)
		for _, extra := range usage.Additional {
			scope := normalizeName(extra.Name)
			if scope == "" {
				scope = normalizeName(extra.Feature)
			}
			if scope != "" {
				q.addCodexLimit(extra.RateLimit, scope, now)
			}
		}
	case "claude", "anthropic":
		var usage map[string]json.RawMessage
		if err := json.Unmarshal(raw, &usage); err != nil {
			return quotaSnapshot{}, fmt.Errorf("decode Claude usage")
		}
		for key, value := range usage {
			kind, scope := otherWindow, ""
			switch {
			case key == "five_hour":
				kind = fiveHour
			case key == "seven_day":
				kind = sevenDay
			case strings.HasPrefix(key, "seven_day_"):
				kind, scope = sevenDay, normalizeName(strings.TrimPrefix(key, "seven_day_"))
			case key == "iguana_necktie":
				kind, scope = sevenDay, "fable"
			default:
				continue
			}
			if string(value) == "null" {
				continue
			}
			var window claudeWindow
			if err := json.Unmarshal(value, &window); err != nil {
				return quotaSnapshot{}, fmt.Errorf("decode Claude quota window")
			}
			if key == "iguana_necktie" && (window.LimitDollars != nil || window.UsedDollars != nil || window.RemainingDollars != nil) {
				continue
			}
			q.windows = append(q.windows, quotaWindow{kind: kind, scope: scope, remaining: remainingPercent(window.Utilization), reset: parseResetTime(window.ResetAt)})
		}
		if err := q.addClaudeFable(usage["limits"]); err != nil {
			return quotaSnapshot{}, err
		}
	default:
		return quotaSnapshot{}, fmt.Errorf("unsupported quota provider")
	}
	return q, nil
}

func (q *quotaSnapshot) addCodexLimit(limit *codexLimit, scope string, now time.Time) {
	if limit == nil {
		return
	}
	for _, window := range []*codexWindow{limit.Primary, limit.Secondary} {
		if window == nil {
			continue
		}
		kind := otherWindow
		switch window.Seconds {
		case 5 * 3600:
			kind = fiveHour
		case 7 * 86400:
			kind = sevenDay
		default:
			continue
		}
		remaining := remainingPercent(window.UsedPercent)
		if limit.Reached || (limit.Allowed != nil && !*limit.Allowed) {
			remaining = 0
		}
		reset := parseResetTime(window.ResetAt)
		if reset.IsZero() && window.ResetAfter != nil {
			after := *window.ResetAfter
			if after > 0 && !math.IsNaN(after) && !math.IsInf(after, 0) && after <= float64((365*24*time.Hour)/time.Second) {
				reset = now.Add(time.Duration(after * float64(time.Second)))
			}
		}
		q.windows = append(q.windows, quotaWindow{kind: kind, scope: scope, remaining: remaining, reset: reset})
	}
}

func (q *quotaSnapshot) addClaudeFable(raw json.RawMessage) error {
	var limits []struct {
		Kind    string          `json:"kind"`
		Percent *float64        `json:"percent"`
		ResetAt json.RawMessage `json:"resets_at"`
		Active  bool            `json:"is_active"`
		Scope   struct {
			Model struct {
				Name string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &limits); err != nil {
		return fmt.Errorf("decode Claude scoped limits")
	}
	var selected *quotaWindow
	for _, limit := range limits {
		name := normalizeName(limit.Scope.Model.Name)
		if limit.Kind != "weekly_scoped" || (name != "fable" && name != "fable_5") {
			continue
		}
		window := quotaWindow{kind: sevenDay, scope: "fable", remaining: remainingPercent(limit.Percent), reset: parseResetTime(limit.ResetAt)}
		if selected == nil || limit.Active {
			selected = &window
		}
		if limit.Active {
			break
		}
	}
	if selected != nil {
		filtered := q.windows[:0]
		for _, window := range q.windows {
			if window.scope != "fable" {
				filtered = append(filtered, window)
			}
		}
		q.windows = append(filtered, *selected)
	}
	return nil
}

func remainingPercent(used *float64) float64 {
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 100 {
		return -1
	}
	return 1 - *used/100
}

func normalizeName(value string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '_'
	}, strings.TrimSpace(value)), "_")
}

func parseResetTime(raw json.RawMessage) time.Time {
	var value string
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	} else {
		value = string(raw)
	}
	stamp, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(stamp) || math.IsInf(stamp, 0) || stamp <= 0 {
		return time.Time{}
	}
	if stamp > 1e11 {
		stamp /= 1000
	}
	if stamp > 253402300799 {
		return time.Time{}
	}
	seconds, fraction := math.Modf(stamp)
	return time.Unix(int64(seconds), int64(fraction*1e9)).UTC()
}

func (q quotaSnapshot) usable(model string, now time.Time, ttl time.Duration) (fiveReset, weekReset time.Time, ok bool) {
	if q.fetchedAt.IsZero() || now.Before(q.fetchedAt) || (ttl > 0 && now.Sub(q.fetchedAt) >= ttl) {
		return time.Time{}, time.Time{}, false
	}
	found := [3]bool{}
	model = strings.ReplaceAll(normalizeName(model), "_", "")
	for _, window := range q.windows {
		if !window.appliesTo(model) {
			continue
		}
		if window.scope == "" {
			found[window.kind] = true
		}
		if window.remaining <= 0 || (!window.reset.IsZero() && !window.reset.After(now)) {
			return time.Time{}, time.Time{}, false
		}
		if window.kind == fiveHour && !window.reset.IsZero() && (fiveReset.IsZero() || window.reset.Before(fiveReset)) {
			fiveReset = window.reset
		}
		if window.kind == sevenDay && !window.reset.IsZero() && (weekReset.IsZero() || window.reset.Before(weekReset)) {
			weekReset = window.reset
		}
	}
	return fiveReset, weekReset, found[fiveHour] && found[sevenDay]
}

func (q quotaSnapshot) nextReset(model string, now time.Time) time.Time {
	var next time.Time
	model = strings.ReplaceAll(normalizeName(model), "_", "")
	for _, window := range q.windows {
		if !window.appliesTo(model) {
			continue
		}
		if window.reset.After(now) && (next.IsZero() || window.reset.Before(next)) {
			next = window.reset
		}
	}
	return next
}

func (w quotaWindow) appliesTo(normalizedModel string) bool {
	scope := strings.ReplaceAll(w.scope, "_", "")
	return scope == "" || strings.Contains(normalizedModel, scope)
}
