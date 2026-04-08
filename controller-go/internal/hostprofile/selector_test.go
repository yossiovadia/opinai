package hostprofile

import (
	"strings"
	"testing"
)

func TestParseRequirements(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		j := `{"language":"python","needs_glibc":true,"heavy_deps":["numpy","tokenizers"],"min_ram_mb":1024}`
		req := ParseRequirements(j)
		if req == nil {
			t.Fatal("ParseRequirements returned nil")
		}
		if req.Language != "python" {
			t.Errorf("Language = %q, want %q", req.Language, "python")
		}
		if !req.NeedsGLibC {
			t.Error("NeedsGLibC = false, want true")
		}
		if len(req.HeavyDeps) != 2 {
			t.Errorf("HeavyDeps = %v, want 2 items", req.HeavyDeps)
		}
		if req.MinRAMMb != 1024 {
			t.Errorf("MinRAMMb = %d, want 1024", req.MinRAMMb)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		if req := ParseRequirements(""); req != nil {
			t.Error("expected nil for empty string")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		if req := ParseRequirements("{invalid}"); req != nil {
			t.Error("expected nil for invalid JSON")
		}
	})
}

func TestSelectImage_PythonWithCDeps(t *testing.T) {
	req := &RuntimeRequirements{
		Language:   "python",
		NeedsGLibC: true,
		HeavyDeps:  []string{"numpy", "tokenizers"},
		MinRAMMb:   512,
	}
	host := &HostProfile{Mode: "local", CPUCores: 8, RAMMb: 16384, LibC: "glibc"}
	sel := SelectImage(req, host, "opinai-controller:latest")

	if sel.Image != ImagePython {
		t.Errorf("Image = %q, want %q", sel.Image, ImagePython)
	}
	if !sel.Feasible {
		t.Errorf("Feasible = false, want true")
	}
	if !strings.Contains(sel.Reason, "debian-slim") {
		t.Errorf("Reason = %q, should mention debian-slim", sel.Reason)
	}
}

func TestSelectImage_PythonSimple(t *testing.T) {
	req := &RuntimeRequirements{
		Language:   "python",
		NeedsGLibC: false,
	}
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192}
	sel := SelectImage(req, host, "opinai-controller:latest")

	// Simple Python — should use default
	if sel.Image != "opinai-controller:latest" {
		t.Errorf("Image = %q, want default", sel.Image)
	}
	if !sel.Feasible {
		t.Error("Feasible = false, want true")
	}
}

func TestSelectImage_GoProject(t *testing.T) {
	req := &RuntimeRequirements{Language: "go"}
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192}
	sel := SelectImage(req, host, "opinai-runner:latest")

	if sel.Image != "opinai-runner:latest" {
		t.Errorf("Image = %q, want default alpine runner", sel.Image)
	}
	if !sel.Feasible {
		t.Error("Feasible = false, want true")
	}
	if !strings.Contains(sel.Reason, "alpine") {
		t.Errorf("Reason = %q, should mention alpine", sel.Reason)
	}
}

func TestSelectImage_GPURequired_NoGPU(t *testing.T) {
	req := &RuntimeRequirements{
		Language:     "python",
		NeedsGPU:     true,
		GPUCount:     1,
		MinGPUVRAMMb: 8000,
	}
	host := &HostProfile{Mode: "local", CPUCores: 8, RAMMb: 32768} // no GPU

	sel := SelectImage(req, host, "opinai-runner:latest")
	if sel.Feasible {
		t.Error("Feasible = true, want false (no GPU)")
	}
	if !strings.Contains(sel.Reason, "no GPU") {
		t.Errorf("Reason = %q, should mention no GPU", sel.Reason)
	}
}

func TestSelectImage_GPURequired_InsufficientVRAM(t *testing.T) {
	req := &RuntimeRequirements{
		Language:     "python",
		NeedsGPU:     true,
		MinGPUVRAMMb: 80000, // 80GB
	}
	host := &HostProfile{
		Mode:         "local",
		CPUCores:     8,
		RAMMb:        32768,
		GPUs:         []GPU{{Name: "RTX 4090", VRAMMb: 24576}},
		TotalGPUVRAM: 24576,
	}
	sel := SelectImage(req, host, "opinai-runner:latest")
	if sel.Feasible {
		t.Error("Feasible = true, want false (insufficient VRAM)")
	}
	if !strings.Contains(sel.Reason, "VRAM") {
		t.Errorf("Reason = %q, should mention VRAM", sel.Reason)
	}
}

func TestSelectImage_GPURequired_MultiGPU(t *testing.T) {
	req := &RuntimeRequirements{
		Language: "python",
		NeedsGPU: true,
		GPUCount: 4,
	}
	host := &HostProfile{
		Mode:     "local",
		CPUCores: 16,
		RAMMb:    65536,
		GPUs:     []GPU{{Name: "A100"}, {Name: "A100"}}, // only 2
	}
	sel := SelectImage(req, host, "opinai-runner:latest")
	if sel.Feasible {
		t.Error("Feasible = true, want false (insufficient GPU count)")
	}
	if !strings.Contains(sel.Reason, "requires 4 GPUs") {
		t.Errorf("Reason = %q, should mention GPU count", sel.Reason)
	}
}

func TestSelectImage_InsufficientRAM(t *testing.T) {
	req := &RuntimeRequirements{
		Language: "python",
		MinRAMMb: 32768, // 32GB
	}
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192} // 8GB

	sel := SelectImage(req, host, "opinai-runner:latest")
	if sel.Feasible {
		t.Error("Feasible = true, want false (insufficient RAM)")
	}
	if !strings.Contains(sel.Reason, "RAM") {
		t.Errorf("Reason = %q, should mention RAM", sel.Reason)
	}
}

func TestSelectImage_InsufficientCPU(t *testing.T) {
	req := &RuntimeRequirements{
		Language:    "python",
		MinCPUCores: 16,
	}
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192}

	sel := SelectImage(req, host, "opinai-runner:latest")
	if sel.Feasible {
		t.Error("Feasible = true, want false (insufficient CPU)")
	}
	if !strings.Contains(sel.Reason, "CPU") {
		t.Errorf("Reason = %q, should mention CPU", sel.Reason)
	}
}

func TestSelectImage_NilRequirements(t *testing.T) {
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192}
	sel := SelectImage(nil, host, "opinai-runner:latest")
	if sel.Image != "opinai-runner:latest" {
		t.Errorf("Image = %q, want default", sel.Image)
	}
	if !sel.Feasible {
		t.Error("Feasible = false, want true for nil requirements")
	}
}

func TestSelectImage_NilHost(t *testing.T) {
	req := &RuntimeRequirements{Language: "python", NeedsGLibC: true, HeavyDeps: []string{"numpy"}}
	sel := SelectImage(req, nil, "opinai-runner:latest")
	// Should still select image correctly, just can't check feasibility
	if sel.Image != ImagePython {
		t.Errorf("Image = %q, want %q", sel.Image, ImagePython)
	}
	if !sel.Feasible {
		t.Error("Feasible = false, want true (no host to check against)")
	}
}

func TestSelectImage_ResourceSizing(t *testing.T) {
	req := &RuntimeRequirements{
		Language:    "python",
		MinCPUCores: 2,
		MinRAMMb:    2048,
	}
	host := &HostProfile{Mode: "local", CPUCores: 8, RAMMb: 16384}
	sel := SelectImage(req, host, "opinai-runner:latest")

	if sel.CPUReq != "1000m" {
		t.Errorf("CPUReq = %q, want %q", sel.CPUReq, "1000m")
	}
	if sel.CPULim != "2000m" {
		t.Errorf("CPULim = %q, want %q", sel.CPULim, "2000m")
	}
	if sel.MemReq != "2048Mi" {
		t.Errorf("MemReq = %q, want %q", sel.MemReq, "2048Mi")
	}
	if sel.MemLim != "4096Mi" {
		t.Errorf("MemLim = %q, want %q", sel.MemLim, "4096Mi")
	}
}

func TestRegistryPrefix(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"opinai-controller:latest", ""},
		{"opinai-runner:latest", ""},
		{"registry.example.com/ns/opinai-controller:latest", "registry.example.com/ns/"},
		{"image-registry.openshift-image-registry.svc:5000/opinai/opinai-controller:latest", "image-registry.openshift-image-registry.svc:5000/opinai/"},
		{"ghcr.io/user/opinai:v1", "ghcr.io/user/"},
	}
	for _, tt := range tests {
		got := registryPrefix(tt.image)
		if got != tt.want {
			t.Errorf("registryPrefix(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestSelectImage_RegistryPrefix(t *testing.T) {
	req := &RuntimeRequirements{Language: "python", NeedsGLibC: true, HeavyDeps: []string{"numpy"}}
	host := &HostProfile{Mode: "kubernetes", CPUCores: 8, RAMMb: 16384}

	// When base image has a registry prefix, the selected python image should use the same prefix
	sel := SelectImage(req, host, "registry.example.com/ns/opinai-controller:latest")
	want := "registry.example.com/ns/" + ImagePython
	if sel.Image != want {
		t.Errorf("Image = %q, want %q", sel.Image, want)
	}
}

func TestSelectImage_GLibCNeeded_UnknownLanguage(t *testing.T) {
	req := &RuntimeRequirements{
		Language:   "ruby",
		NeedsGLibC: true,
	}
	host := &HostProfile{Mode: "local", CPUCores: 4, RAMMb: 8192}
	sel := SelectImage(req, host, "opinai-runner:latest")

	if sel.Image != ImagePython {
		t.Errorf("Image = %q, want %q for glibc-needing project", sel.Image, ImagePython)
	}
}
