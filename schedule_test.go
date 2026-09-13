package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// testNow is Monday 2026-09-14 10:30 UTC. UTC keeps DST out of the math.
var testNow = time.Date(2026, time.September, 14, 10, 30, 0, 0, time.UTC)

func sep(day, hour, minute int) time.Time {
	return time.Date(2026, time.September, day, hour, minute, 0, 0, time.UTC)
}

func TestNextDaily(t *testing.T) {
	cases := []struct {
		name, time string
		want       time.Time
	}{
		{"later today", "11:00", sep(14, 11, 0)},
		{"earlier today rolls to tomorrow", "09:00", sep(15, 9, 0)},
		{"exactly now rolls to tomorrow", "10:30", sep(15, 10, 30)},
		{"midnight", "00:00", sep(15, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextDaily(testNow, tc.time)
			if err != nil {
				t.Fatalf("nextDaily(%q): %v", tc.time, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("nextDaily(%q) = %s, want %s", tc.time, got, tc.want)
			}
		})
	}
}

func TestParseTime_Invalid(t *testing.T) {
	for _, in := range []string{"", "9", "ab:00", "12:cd", "24:00", "12:60", "-1:00"} {
		if _, _, err := parseTime(in); err == nil {
			t.Errorf("parseTime(%q) = nil error, want error", in)
		}
	}
}

func TestNextHourly(t *testing.T) {
	cases := []struct {
		name   string
		now    time.Time
		minute int
		want   time.Time
	}{
		{"later this hour", testNow, 45, sep(14, 10, 45)},
		{"earlier rolls to next hour", testNow, 15, sep(14, 11, 15)},
		{"exactly now rolls to next hour", testNow, 30, sep(14, 11, 30)},
		{"rolls past midnight", sep(14, 23, 50), 10, sep(15, 0, 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextHourly(tc.now, tc.minute); !got.Equal(tc.want) {
				t.Errorf("nextHourly(%s, %d) = %s, want %s", tc.now, tc.minute, got, tc.want)
			}
		})
	}
}

func TestNextWeekly(t *testing.T) {
	if testNow.Weekday() != time.Monday {
		t.Fatalf("testNow is %s, cases assume Monday", testNow.Weekday())
	}
	cases := []struct {
		name, day, time string
		want            time.Time
	}{
		{"later today", "monday", "11:00", sep(14, 11, 0)},
		{"earlier today rolls a week", "monday", "09:00", sep(21, 9, 0)},
		{"later this week, any case", "Wednesday", "09:00", sep(16, 9, 0)},
		{"end of week", "sunday", "10:00", sep(20, 10, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextWeekly(testNow, tc.day, tc.time)
			if err != nil {
				t.Fatalf("nextWeekly(%q, %q): %v", tc.day, tc.time, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("nextWeekly(%q, %q) = %s, want %s", tc.day, tc.time, got, tc.want)
			}
		})
	}

	if _, err := nextWeekly(testNow, "funday", "09:00"); err == nil {
		t.Error("nextWeekly with unknown day = nil error, want error")
	}
}

func TestNextCron(t *testing.T) {
	cases := []struct {
		expr string
		want time.Time
	}{
		{"0 12 * * *", sep(14, 12, 0)},
		{"0 9 * * *", sep(15, 9, 0)},
		{"45 * * * *", sep(14, 10, 45)},
	}
	for _, tc := range cases {
		got, err := nextCron(testNow, tc.expr)
		if err != nil {
			t.Errorf("nextCron(%q): %v", tc.expr, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("nextCron(%q) = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

func TestNextCron_RejectsUnsupported(t *testing.T) {
	for _, expr := range []string{
		"*/5 * * * *",  // step
		"0 9-17 * * *", // range
		"0,30 * * * *", // list
		"0 0 * * 1",    // day-of-week
		"0 0 1 * *",    // day-of-month
		"* 9 * * *",    // wildcard minute
		"0 9 * *",      // four fields
		"60 9 * * *",   // minute out of range
		"0 24 * * *",   // hour out of range
	} {
		if _, err := nextCron(testNow, expr); err == nil {
			t.Errorf("nextCron(%q) = nil error, want error", expr)
		}
	}
}

func TestCalculateNextRun_RejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{}`,
		`{"type":"bogus"}`,
		`{"type":"on_demand"}`,
		`{"type":"hourly","minute":60}`,
		`{"type":"interval","minutes":0}`,
		`{"type":"daily","time":"25:00"}`,
		`{"type":"once","at":"tomorrow"}`,
	} {
		if _, err := CalculateNextRun(json.RawMessage(raw)); err == nil {
			t.Errorf("CalculateNextRun(%s) = nil error, want error", raw)
		}
	}
}

func TestCalculateNextRun_Interval(t *testing.T) {
	got, err := CalculateNextRun(json.RawMessage(`{"type":"interval","minutes":15}`))
	if err != nil {
		t.Fatalf("CalculateNextRun: %v", err)
	}
	if drift := time.Until(got) - 15*time.Minute; drift < -2*time.Second || drift > 2*time.Second {
		t.Errorf("interval next run is %s from now, want ~15m", time.Until(got))
	}
}

func TestSchedule_OncePastIsRunnableButNotCreatable(t *testing.T) {
	past := json.RawMessage(`{"type":"once","at":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`)
	future := json.RawMessage(`{"type":"once","at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`)

	next, err := CalculateNextRun(past)
	if err != nil {
		t.Fatalf("CalculateNextRun(past once): %v", err)
	}
	if next.After(time.Now()) {
		t.Errorf("CalculateNextRun(past once) = %s, want the past time", next)
	}

	if err := ValidateSchedule(past); err == nil || !strings.Contains(err.Error(), "in the past") {
		t.Errorf("ValidateSchedule(past once) = %v, want 'in the past'", err)
	}
	if err := ValidateSchedule(future); err != nil {
		t.Errorf("ValidateSchedule(future once) = %v, want nil", err)
	}
}

func TestValidateSchedule_OnDemand(t *testing.T) {
	if err := ValidateSchedule(json.RawMessage(`{"type":"on_demand"}`)); err != nil {
		t.Errorf("ValidateSchedule(on_demand) = %v, want nil", err)
	}
}
