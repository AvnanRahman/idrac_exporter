package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExpandTargets(t *testing.T) {
	got, err := expandTargets([]string{"10.0.0.8-10", "host.example.com", " 10.0.0.9 ", "", "10.0.1.1-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.8", "10.0.0.9", "10.0.0.10", "host.example.com", "10.0.1.1"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, bad := range []string{"10.0.0.10-8", "10.0.0.1-256"} {
		if _, err := expandTargets([]string{bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

const groupConfig = `
hosts:
  default:
    username: def
    password: def
  10.0.9.1:
    username: host
    password: host
groups:
  lenovo:
    username: USERID
    password: a
    targets: [10.0.1.11-13]
  pesso:
    username: pesso
    password: b
    targets:
      - 10.0.2.11-12
  nolist:
    username: x
    password: c
metrics:
  all:
    enabled: true
`

func loadConfig(t *testing.T, data string) (*RootConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewConfig()
	if err := c.FromFile(path, false); err != nil {
		t.Fatal(err)
	}
	return c, c.Validate()
}

func TestGroupTargets(t *testing.T) {
	c, err := loadConfig(t, groupConfig)
	if err != nil {
		t.Fatal(err)
	}
	SetConfig(c)

	if !c.Groups["pesso"].Metrics.PCIe.Enabled {
		t.Error("groups must inherit the global metrics configuration")
	}

	tests := []struct{ target, group, user string }{
		{"10.0.1.12", "", "USERID"},     // from group targets
		{"10.0.2.11", "", "pesso"},      // from group targets
		{"10.0.9.1", "", "host"},        // hosts entry
		{"10.0.7.7", "", "def"},         // default
		{"10.0.1.12", "pesso", "pesso"}, // explicit group parameter wins
		{"10.0.7.7", "nolist", "x"},
	}
	for _, tc := range tests {
		a := GetAuthConfig(tc.target, tc.group)
		if a == nil || a.Username != tc.user {
			t.Errorf("GetAuthConfig(%q, %q) = %v, want user %q", tc.target, tc.group, a, tc.user)
		}
	}
	if GetAuthConfig("10.0.1.12", "missing") != nil {
		t.Error("unknown group must not fall back to other credentials")
	}

	var items []DiscoverItem
	if err := json.Unmarshal([]byte(GetDiscover()), &items); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{}
	for _, it := range items {
		if it.Labels["group"] != it.Labels["__param_group"] {
			t.Errorf("inconsistent labels: %v", it.Labels)
		}
		groups[it.Labels["group"]] = append(groups[it.Labels["group"]], it.Targets...)
	}
	if !slices.Equal(groups["lenovo"], []string{"10.0.1.11", "10.0.1.12", "10.0.1.13"}) {
		t.Errorf("lenovo targets: %v", groups["lenovo"])
	}
	if len(groups["pesso"]) != 2 || groups["nolist"] != nil {
		t.Errorf("unexpected discover groups: %v", groups)
	}
	if !slices.Equal(groups[""], []string{"10.0.9.1"}) {
		t.Errorf("hosts must still be discovered without labels: %v", groups[""])
	}
}

func TestGroupTargetConflicts(t *testing.T) {
	dupGroup := strings.Replace(groupConfig, "10.0.2.11-12", "10.0.1.13-14", 1)
	if _, err := loadConfig(t, dupGroup); err == nil || !strings.Contains(err.Error(), "10.0.1.13") {
		t.Errorf("expected duplicate target error, got %v", err)
	}

	dupHost := strings.Replace(groupConfig, "10.0.2.11-12", "10.0.9.1", 1)
	if _, err := loadConfig(t, dupHost); err == nil || !strings.Contains(err.Error(), "under hosts") {
		t.Errorf("expected host conflict error, got %v", err)
	}
}
