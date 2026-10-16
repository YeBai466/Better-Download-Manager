package config

import "testing"

func TestValidateRejectsInvalidRuntimeBounds(t *testing.T) {
	for _, mutate := range []func(*Settings){
		func(s *Settings) { s.MaxConcurrent = 21 }, func(s *Settings) { s.Connections = 33 },
		func(s *Settings) { s.SpeedLimit = -1 }, func(s *Settings) { s.TakeoverPort = 65536 },
	} {
		s := Default()
		mutate(&s)
		if s.Validate() == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}
