package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ScheduleType extracts the "type" field from a schedule JSON blob.
func ScheduleType(raw json.RawMessage) (string, error) {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return "", fmt.Errorf("parse schedule type: %w", err)
	}
	if base.Type == "" {
		return "", fmt.Errorf("schedule type is required")
	}
	return base.Type, nil
}

// ValidateSchedule checks that a schedule JSON blob is well-formed and has
// valid parameters. Returns nil if the schedule is valid.
func ValidateSchedule(raw json.RawMessage) error {
	st, err := ScheduleType(raw)
	if err != nil {
		return err
	}
	if st == "on_demand" {
		return nil
	}
	_, err = CalculateNextRun(raw)
	return err
}

// CalculateNextRun computes the next execution time for a schedule.
func CalculateNextRun(scheduleRaw json.RawMessage) (time.Time, error) {
	st, err := ScheduleType(scheduleRaw)
	if err != nil {
		return time.Time{}, err
	}

	now := time.Now()

	switch st {
	case "daily":
		var s DailySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse daily schedule: %w", err)
		}
		return nextDaily(now, s.Time)

	case "hourly":
		var s HourlySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse hourly schedule: %w", err)
		}
		if s.Minute < 0 || s.Minute > 59 {
			return time.Time{}, fmt.Errorf("hourly minute %d out of range 0-59", s.Minute)
		}
		return nextHourly(now, s.Minute), nil

	case "interval":
		var s IntervalSchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse interval schedule: %w", err)
		}
		if s.Minutes <= 0 {
			return time.Time{}, fmt.Errorf("interval minutes must be positive, got %d", s.Minutes)
		}
		return now.Add(time.Duration(s.Minutes) * time.Minute), nil

	case "weekly":
		var s WeeklySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse weekly schedule: %w", err)
		}
		return nextWeekly(now, s.Day, s.Time)

	case "cron":
		var s CronSchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse cron schedule: %w", err)
		}
		return nextCron(now, s.Expression)

	case "once":
		var s OnceSchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse once schedule: %w", err)
		}
		t, err := time.Parse(time.RFC3339, s.At)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse once schedule 'at': %w", err)
		}
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("once schedule 'at' is in the past: %s", s.At)
		}
		return t, nil

	case "on_demand":
		return time.Time{}, fmt.Errorf("on_demand tasks are not scheduled; use RunTaskNow")

	default:
		return time.Time{}, fmt.Errorf("unknown schedule type: %s", st)
	}
}

func parseTime(timeStr string) (int, int, error) {
	parts := strings.SplitN(timeStr, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time format %q (expected HH:MM)", timeStr)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid hour %q in time %q", parts[0], timeStr)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid minute %q in time %q", parts[1], timeStr)
	}
	if h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("hour %d out of range 0-23 in time %q", h, timeStr)
	}
	if m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("minute %d out of range 0-59 in time %q", m, timeStr)
	}
	return h, m, nil
}

func nextDaily(now time.Time, timeStr string) (time.Time, error) {
	h, m, err := parseTime(timeStr)
	if err != nil {
		return time.Time{}, err
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return next, nil
}

func nextHourly(now time.Time, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}

var dayMap = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

func nextWeekly(now time.Time, day, timeStr string) (time.Time, error) {
	targetDay, ok := dayMap[strings.ToLower(day)]
	if !ok {
		return time.Time{}, fmt.Errorf("invalid day %q (expected monday, tuesday, etc.)", day)
	}
	h, m, err := parseTime(timeStr)
	if err != nil {
		return time.Time{}, err
	}

	// Move to the target day of the week.
	daysUntil := int(targetDay) - int(now.Weekday())
	if daysUntil < 0 {
		daysUntil += 7
	}

	next := time.Date(now.Year(), now.Month(), now.Day()+daysUntil, h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(next.Year(), next.Month(), next.Day()+7, h, m, 0, 0, next.Location())
	}
	return next, nil
}

func nextCron(now time.Time, expression string) (time.Time, error) {
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return time.Time{}, fmt.Errorf("cron expression requires exactly 5 fields (min hour dom month dow), got %d: %q", len(parts), expression)
	}

	minute, hour, dom, month, dow := parts[0], parts[1], parts[2], parts[3], parts[4]

	// Reject syntax we can't correctly evaluate — step values, lists, ranges.
	for _, field := range parts {
		if strings.ContainsAny(field, "/,-") {
			return time.Time{}, fmt.Errorf("step (/), range (-), and list (,) syntax not supported in cron expression: %q; use 'interval', 'weekly', or 'daily' schedule types instead", expression)
		}
	}

	// Only wildcard is supported for day-of-month, month, and day-of-week.
	if dom != "*" || month != "*" || dow != "*" {
		return time.Time{}, fmt.Errorf("day-of-month, month, and day-of-week constraints not supported: %q; use 'weekly' or 'daily' schedule types instead", expression)
	}

	if minute == "*" {
		return time.Time{}, fmt.Errorf("wildcard minute not supported: %q; use 'interval' schedule type for sub-hour frequencies", expression)
	}

	m, err := strconv.Atoi(minute)
	if err != nil || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("invalid minute %q in cron expression (must be 0-59)", minute)
	}

	if hour == "*" {
		// Specific minute, any hour: hourly.
		return nextHourly(now, m), nil
	}

	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return time.Time{}, fmt.Errorf("invalid hour %q in cron expression (must be 0-23)", hour)
	}

	// Specific minute and hour: daily.
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return next, nil
}
