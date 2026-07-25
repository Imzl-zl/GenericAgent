package application

import (
	"testing"
)

func TestSchedulerConfigDirFor(t *testing.T) {
	cases := []struct {
		name      string
		scoped    bool
		root      string
		session   string
		want      string
	}{
		{
			name:    "global loopback config",
			scoped:  false,
			root:    "/ga/config",
			session: "session-1",
			want:    "/ga/config",
		},
		{
			name:    "session-scoped container config",
			scoped:  true,
			root:    "/ga/config",
			session: "session-1",
			want:    "/ga/config/session-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scheduler{cfg: SchedulerConfig{
				ConfigRoot:          tc.root,
				SessionScopedConfig: tc.scoped,
			}}
			got := s.configDirFor(tc.session)
			if got != tc.want {
				t.Fatalf("configDirFor() = %q, want %q", got, tc.want)
			}
		})
	}
}
