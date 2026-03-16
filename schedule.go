package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CalculateNextRun computes the next execution time for a schedule.
func CalculateNextRun(scheduleRaw json.RawMessage) (time.Time, error) {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(scheduleRaw, &base); err != nil {
		return time.Time{}, fmt.Errorf("parse schedule type: %w", err)
	}

	now := time.Now()

	switch base.Type {
	case "daily":
		var s DailySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse daily schedule: %w", err)
		}
		return nextDaily(now, s.Time), nil

	case "hourly":
		var s HourlySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse hourly schedule: %w", err)
		}
		return nextHourly(now, s.Minute), nil

	case "interval":
		var s IntervalSchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse interval schedule: %w", err)
		}
		if s.Minutes <= 0 {
			s.Minutes = 60
		}
		return now.Add(time.Duration(s.Minutes) * time.Minute), nil

	case "weekly":
		var s WeeklySchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse weekly schedule: %w", err)
		}
		return nextWeekly(now, s.Day, s.Time), nil

	case "cron":
		var s CronSchedule
		if err := json.Unmarshal(scheduleRaw, &s); err != nil {
			return time.Time{}, fmt.Errorf("parse cron schedule: %w", err)
		}
		return nextCron(now, s.Expression), nil

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
		return now, nil

	default:
		return time.Time{}, fmt.Errorf("unknown schedule type: %s", base.Type)
	}
}

func parseTime(timeStr string) (hour, minute int) {
	parts := strings.SplitN(timeStr, ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h, m
}

func nextDaily(now time.Time, timeStr string) time.Time {
	h, m := parseTime(timeStr)
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return next
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

func nextWeekly(now time.Time, day, timeStr string) time.Time {
	targetDay, ok := dayMap[strings.ToLower(day)]
	if !ok {
		targetDay = time.Monday
	}
	h, m := parseTime(timeStr)

	// Move to the target day of the week.
	daysUntil := int(targetDay) - int(now.Weekday())
	if daysUntil < 0 {
		daysUntil += 7
	}

	next := time.Date(now.Year(), now.Month(), now.Day()+daysUntil, h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(next.Year(), next.Month(), next.Day()+7, h, m, 0, 0, next.Location())
	}
	return next
}

func nextCron(now time.Time, expression string) time.Time {
	parts := strings.Fields(expression)
	if len(parts) < 2 {
		return now.Add(time.Hour)
	}

	minute := parts[0]
	hour := parts[1]

	if minute != "*" && hour != "*" {
		// Specific minute and hour: treat as daily.
		m, _ := strconv.Atoi(minute)
		h, _ := strconv.Atoi(hour)
		next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if !next.After(now) {
			next = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
		}
		return next
	}

	if minute != "*" && hour == "*" {
		// Specific minute, any hour: treat as hourly.
		m, _ := strconv.Atoi(minute)
		return nextHourly(now, m)
	}

	// Fallback: run in 1 hour.
	return now.Add(time.Hour)
}
