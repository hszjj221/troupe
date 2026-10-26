package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `default_agent: codex
roles:
  leader: codex
  coder: claude
  reviewer: codex
agents:
  codex:
    binary: /custom/codex
    extra: ignored
  claude:
    binary: claude
extra:
  harmless: true
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	want := Config{
		DefaultAgent: "codex",
		Roles:        map[string]string{"leader": "codex", "coder": "claude", "reviewer": "codex"},
		Agents:       map[string]AgentConfig{"codex": {Binary: "/custom/codex"}, "claude": {Binary: "claude"}},
	}
	if got, err := Load(path, true); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %#v, %v; want %#v", got, err, want)
	}
}

func TestLoadEmptyAndErrors(t *testing.T) {
	root := t.TempDir()
	for _, tt := range []struct {
		name     string
		data     string
		missing  bool
		explicit bool
		wantErr  string
	}{
		{name: "empty file", explicit: true},
		{name: "optional missing", missing: true},
		{name: "explicit missing", missing: true, explicit: true, wantErr: "read config"},
		{name: "invalid YAML", data: "roles: [", wantErr: "decode config"},
		{name: "invalid shape", data: "roles: []", wantErr: "decode config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, tt.name+".yaml")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Load(path, tt.explicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load = %#v, %v; want %q", got, err, tt.wantErr)
				}
				if tt.missing && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Load error = %v; want wrapped not-exist error", err)
				}
			} else if err != nil || !reflect.DeepEqual(got, Config{}) {
				t.Fatalf("Load = %#v, %v; want empty config", got, err)
			}
		})
	}
	for _, explicit := range []bool{false, true} {
		if _, err := Load(root, explicit); err == nil {
			t.Fatalf("Load(directory, %v) succeeded; want read error", explicit)
		}
	}
}

func TestLoadUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files regardless of permission bits")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("default_agent: codex"), 0); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		if _, err := Load(path, explicit); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("Load(unreadable, %v) error = %v; want permission error", explicit, err)
		}
	}
}

func TestResolvePath(t *testing.T) {
	for _, tt := range []struct {
		name, flag, env, home string
		local                 bool
		want                  string
		explicit              bool
	}{
		{"flag wins", "flag.yaml", "env.yaml", "home", true, "flag.yaml", true},
		{"environment wins", "", "env.yaml", "home", true, "env.yaml", true},
		{"cwd wins over home", "", "", "home", true, "troupe.yaml", false},
		{"cwd wins over default", "", "", "", true, "troupe.yaml", false},
		{"home fallback", "", "", "home", false, filepath.Join("home", "config.yaml"), false},
		{"default fallback", "", "", "", false, filepath.Join(".troupe", "config.yaml"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.local {
				if err := os.WriteFile("troupe.yaml", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if path, explicit := ResolvePath(tt.flag, tt.env, tt.home); path != tt.want || explicit != tt.explicit {
				t.Fatalf("ResolvePath = %q, %v; want %q, %v", path, explicit, tt.want, tt.explicit)
			}
		})
	}
	t.Run("unreadable candidate does not fall back", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.Mkdir("troupe.yaml", 0o700); err != nil {
			t.Fatal(err)
		}
		path, explicit := ResolvePath("", "", "home")
		if path != "troupe.yaml" || explicit {
			t.Fatalf("ResolvePath = %q, %v; want troupe.yaml, false", path, explicit)
		}
		if _, err := Load(path, explicit); err == nil {
			t.Fatal("unreadable config did not fail")
		}
	})
}

func TestResolveAgent(t *testing.T) {
	cfg := Config{DefaultAgent: "codex", Roles: map[string]string{"coder": "claude", "bad": "unknown", "empty": "", "": "claude"}}
	for _, tt := range []struct {
		name, agent, role, want string
		cfg                     Config
		wantErr                 bool
	}{
		{name: "CLI wins over role and default", agent: "codex", role: "coder", cfg: cfg, want: "codex"},
		{name: "CLI without config", agent: "claude", role: "coder", want: "claude"},
		{name: "role wins over default", role: "coder", cfg: cfg, want: "claude"},
		{name: "role without default", role: "coder", cfg: Config{Roles: cfg.Roles}, want: "claude"},
		{name: "unbound role uses default", role: "reviewer", cfg: cfg, want: "codex"},
		{name: "no role uses default", cfg: cfg, want: "codex"},
		{name: "unknown binding is not replaced", role: "bad", cfg: cfg, want: "unknown"},
		{name: "empty binding is not replaced", role: "empty", cfg: cfg},
		{name: "no agent", wantErr: true},
		{name: "unbound role without default", role: "reviewer", cfg: Config{Roles: cfg.Roles}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAgent(tt.cfg, tt.agent, tt.role)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("ResolveAgent = %q, %v; want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
