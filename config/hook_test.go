package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadVolumeHook(t *testing.T) {
	p := writeConfig(t, `
[volumes.photos]
path = "/tmp/photos"

[volumes.photos.hook]
command = ["kopia", "snapshot", "create", "."]
timeout = "30m"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["photos"]
	if v.Hook == nil {
		t.Fatalf("Hook is nil, want resolved")
	}
	want := []string{"kopia", "snapshot", "create", "."}
	if strings.Join(v.Hook.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Command = %v, want %v", v.Hook.Command, want)
	}
	if v.Hook.Timeout != 30*time.Minute {
		t.Fatalf("Timeout = %s, want 30m", v.Hook.Timeout)
	}
}

func TestLoadVolumeHookDefaultTimeout(t *testing.T) {
	p := writeConfig(t, `
[volumes.photos]
path = "/tmp/photos"

[volumes.photos.hook]
command = ["backup.sh"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Volumes["photos"].Hook.Timeout; got != DefaultHookTimeout {
		t.Fatalf("Timeout = %s, want default %s", got, DefaultHookTimeout)
	}
}

func TestLoadVolumeHookNoBlock(t *testing.T) {
	p := writeConfig(t, `
[volumes.photos]
path = "/tmp/photos"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Volumes["photos"].Hook != nil {
		t.Fatalf("Hook = %#v, want nil when no [hook] block", cfg.Volumes["photos"].Hook)
	}
}

func TestLoadVolumeHookErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing command",
			body: "[volumes.v]\npath=\"/tmp/v\"\n[volumes.v.hook]\ntimeout=\"1m\"\n",
			want: "hook.command is required",
		},
		{
			name: "empty command list",
			body: "[volumes.v]\npath=\"/tmp/v\"\n[volumes.v.hook]\ncommand=[]\n",
			want: "hook.command is required",
		},
		{
			name: "empty arg",
			body: "[volumes.v]\npath=\"/tmp/v\"\n[volumes.v.hook]\ncommand=[\"kopia\", \"\"]\n",
			want: "hook.command[1] is empty",
		},
		{
			name: "bad timeout",
			body: "[volumes.v]\npath=\"/tmp/v\"\n[volumes.v.hook]\ncommand=[\"x\"]\ntimeout=\"nope\"\n",
			want: "hook.timeout",
		},
		{
			name: "non-positive timeout",
			body: "[volumes.v]\npath=\"/tmp/v\"\n[volumes.v.hook]\ncommand=[\"x\"]\ntimeout=\"0s\"\n",
			want: "hook.timeout must be a positive duration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
