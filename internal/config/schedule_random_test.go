package config

import "testing"

func TestScheduleCheckinRandomDefaults(t *testing.T) {
	s := Schedule{}
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if s.CheckinMode != "legacy" {
		t.Fatalf("CheckinMode = %q, want legacy", s.CheckinMode)
	}
	if s.CheckinWindowStart != "08:00" || s.CheckinWindowEnd != "09:30" {
		t.Fatalf("window = %s-%s", s.CheckinWindowStart, s.CheckinWindowEnd)
	}
	if s.CheckinRetryStart != "09:30" || s.CheckinRetryEnd != "10:00" {
		t.Fatalf("retry = %s-%s", s.CheckinRetryStart, s.CheckinRetryEnd)
	}
}

func TestScheduleCheckinRandomValidation(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Schedule)
	}{
		{"bad mode", func(s *Schedule) { s.CheckinMode = "nope" }},
		{"bad start", func(s *Schedule) { s.CheckinMode = "random"; s.CheckinWindowStart = "25:00" }},
		{"reversed window", func(s *Schedule) {
			s.CheckinMode = "random"
			s.CheckinWindowStart = "10:00"
			s.CheckinWindowEnd = "09:00"
		}},
		{"reversed retry", func(s *Schedule) {
			s.CheckinMode = "random"
			s.CheckinRetryStart = "10:00"
			s.CheckinRetryEnd = "09:00"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSchedule()
			tc.mod(&s)
			if err := s.Normalize(); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestScheduleCheckinRandomNormalizesMode(t *testing.T) {
	s := DefaultSchedule()
	s.CheckinMode = " RANDOM "
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if s.CheckinMode != "random" {
		t.Fatalf("CheckinMode = %q", s.CheckinMode)
	}
}
