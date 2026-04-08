package hostprofile

import (
	"encoding/json"
	"runtime"
	"testing"
)

func TestDetectLocal(t *testing.T) {
	// Detect with no K8s client — should produce a local profile
	p := Detect(nil)
	if p == nil {
		t.Fatal("Detect returned nil")
	}
	if p.Mode != "local" {
		t.Errorf("Mode = %q, want %q", p.Mode, "local")
	}
	if p.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", p.Arch, runtime.GOARCH)
	}
	if p.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", p.OS, runtime.GOOS)
	}
	if p.CPUCores <= 0 {
		t.Errorf("CPUCores = %d, want > 0", p.CPUCores)
	}
	if p.DetectedAt == "" {
		t.Error("DetectedAt is empty")
	}
}

func TestDetectRAM(t *testing.T) {
	ram := detectRAM()
	// On any real machine this should be > 0 (CI or local)
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if ram <= 0 {
			t.Errorf("detectRAM() = %d, want > 0 on %s", ram, runtime.GOOS)
		}
	}
}

func TestDetectDistroLibC(t *testing.T) {
	distro, libc := detectDistroLibC()
	// On macOS: distro=macos, libc=glibc
	// On Linux: distro from /etc/os-release, libc=glibc or musl
	if runtime.GOOS == "darwin" {
		if distro != "macos" {
			t.Errorf("distro = %q, want %q on darwin", distro, "macos")
		}
		if libc != "glibc" {
			t.Errorf("libc = %q, want %q on darwin", libc, "glibc")
		}
	}
	// On any OS, libc should be non-empty
	if libc == "" {
		t.Error("libc is empty")
	}
}

func TestDetectRuntimes(t *testing.T) {
	runtimes := detectRuntimes()
	// Go should always be detected since we're running a Go test
	found := false
	for _, r := range runtimes {
		if len(r) >= 2 && r[:2] == "go" {
			found = true
		}
	}
	if !found {
		t.Errorf("detectRuntimes() = %v, expected to find go runtime", runtimes)
	}
}

func TestHostProfileJSON(t *testing.T) {
	p := &HostProfile{
		Mode:     "local",
		CPUCores: 8,
		Arch:     "amd64",
		RAMMb:    16384,
		OS:       "linux",
		Distro:   "ubuntu",
		LibC:     "glibc",
		Runtimes: []string{"python3.12", "go1.25"},
	}
	j := p.JSON()
	if j == "" {
		t.Fatal("JSON() returned empty string")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(j), &parsed); err != nil {
		t.Fatalf("JSON() produced invalid JSON: %v", err)
	}
	if parsed["mode"] != "local" {
		t.Errorf("mode = %v, want 'local'", parsed["mode"])
	}
}

func TestHostProfileSummary(t *testing.T) {
	p := &HostProfile{
		Mode:     "local",
		CPUCores: 4,
		Arch:     "amd64",
		RAMMb:    8192,
		OS:       "linux",
		LibC:     "glibc",
		Runtimes: []string{"python3.12"},
	}
	s := p.Summary()
	if s == "" {
		t.Fatal("Summary() returned empty string")
	}
	// Should contain key info
	for _, want := range []string{"mode=local", "cpu=4", "ram=8192", "libc=glibc"} {
		if !contains(s, want) {
			t.Errorf("Summary() = %q, missing %q", s, want)
		}
	}
}

func TestHostProfileSummaryWithGPU(t *testing.T) {
	p := &HostProfile{
		Mode:         "local",
		CPUCores:     8,
		Arch:         "amd64",
		RAMMb:        32768,
		OS:           "linux",
		LibC:         "glibc",
		GPUs:         []GPU{{Index: 0, Name: "RTX 4090", VRAMMb: 24576}},
		TotalGPUVRAM: 24576,
	}
	s := p.Summary()
	if !contains(s, "1x GPU") {
		t.Errorf("Summary() = %q, missing GPU info", s)
	}
	if !contains(s, "RTX 4090") {
		t.Errorf("Summary() = %q, missing GPU name", s)
	}
}

func TestHasGPU(t *testing.T) {
	noGPU := &HostProfile{}
	if noGPU.HasGPU() {
		t.Error("HasGPU() = true for empty GPUs")
	}
	withGPU := &HostProfile{GPUs: []GPU{{Name: "test"}}}
	if !withGPU.HasGPU() {
		t.Error("HasGPU() = false with GPU present")
	}
}

func TestHasGLibC(t *testing.T) {
	tests := []struct {
		libc string
		want bool
	}{
		{"glibc", true},
		{"musl", false},
		{"", false},
	}
	for _, tt := range tests {
		p := &HostProfile{LibC: tt.libc}
		if got := p.HasGLibC(); got != tt.want {
			t.Errorf("HasGLibC() with libc=%q = %v, want %v", tt.libc, got, tt.want)
		}
	}
}

func TestIsInterestingLabel(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"nvidia.com/gpu.product", true},
		{"kubernetes.io/arch", true},
		{"node.kubernetes.io/instance-type", true},
		{"some-gpu-label", true},
		{"app.kubernetes.io/name", false},
		{"hostname", false},
	}
	for _, tt := range tests {
		if got := isInterestingLabel(tt.key); got != tt.want {
			t.Errorf("isInterestingLabel(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestIsBuiltinGroup(t *testing.T) {
	if !isBuiltinGroup("apps") {
		t.Error("apps should be builtin")
	}
	if !isBuiltinGroup("batch") {
		t.Error("batch should be builtin")
	}
	if isBuiltinGroup("mycompany.io") {
		t.Error("mycompany.io should not be builtin")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
