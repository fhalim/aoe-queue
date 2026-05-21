package main

import (
	"testing"
	"time"
)

func TestDetectThrottle(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantMatch bool
		wantReset string
	}{
		{
			name: "detects throttle message",
			output: `Some output before
You've hit your limit · resets 4:30am (UTC)
More output after`,
			wantMatch: true,
			wantReset: "4:30am",
		},
		{
			name: "case insensitive",
			output: `you've hit your limit · resets 5:45pm (UTC)`,
			wantMatch: true,
			wantReset: "5:45pm",
		},
		{
			name: "with space in time",
			output: `You've hit your limit · resets 9:00 am (UTC)`,
			wantMatch: true,
			wantReset: "9:00 am",
		},
		{
			name:      "no throttle",
			output:    `Normal output without throttle`,
			wantMatch: false,
		},
		{
			name:      "empty output",
			output:    ``,
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te, ok := detectThrottle(tt.output)
			if ok != tt.wantMatch {
				t.Errorf("detectThrottle match = %v, want %v", ok, tt.wantMatch)
			}
			if ok && te.ResetAt != tt.wantReset {
				t.Errorf("ResetAt = %v, want %v", te.ResetAt, tt.wantReset)
			}
			if ok && te.WaitDuration <= 0 {
				t.Errorf("WaitDuration = %v, want > 0", te.WaitDuration)
			}
		})
	}
}

func TestThrottleTimeCalculation(t *testing.T) {
	// Test that parse doesn't crash and returns sensible duration
	te, ok := detectThrottle("You've hit your limit · resets 11:59pm (UTC)")
	if !ok {
		t.Fatal("should match throttle pattern")
	}

	// Wait duration should be < 24 hours and > 0
	if te.WaitDuration <= 0 || te.WaitDuration > 24*time.Hour {
		t.Errorf("WaitDuration = %v, want > 0 and < 24h", te.WaitDuration)
	}

	// Test past time (should bump to tomorrow)
	now := time.Now().UTC()
	pastHour := now.Hour() - 1
	if pastHour < 0 {
		pastHour = 23
	}
	output := "You've hit your limit · resets " + time.Date(0, 0, 0, pastHour, 30, 0, 0, time.UTC).Format("3:04pm") + " (UTC)"
	te2, ok := detectThrottle(output)
	if !ok {
		t.Fatal("should match past time")
	}
	// Should be ~23.5 hours away (half that is ~11.75 hours)
	if te2.WaitDuration < 10*time.Hour || te2.WaitDuration > 12*time.Hour {
		t.Errorf("Past time wait = %v, want ~11 hours", te2.WaitDuration)
	}
}

func TestThrottleError(t *testing.T) {
	te := &ThrottleError{
		ResetAt:      "4:30am",
		WaitDuration: 30 * time.Minute,
	}
	msg := te.Error()
	if msg == "" {
		t.Error("Error() returned empty string")
	}
	if !contains(msg, "4:30am") || !contains(msg, "30m") {
		t.Errorf("Error() = %v, missing reset time or duration", msg)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
