package hostprofile

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Runner image names. The controller sets OPINAI_IMAGE; these are the selectable variants.
const (
	ImageDefault = "opinai-runner:latest"         // Alpine-based, good for Go/simple projects
	ImagePython  = "opinai-runner-python:latest"  // Debian-slim based, for Python with C extensions
)

// RuntimeRequirements describes what a project needs to run.
type RuntimeRequirements struct {
	Language      string   `json:"language"`
	NeedsGLibC   bool     `json:"needs_glibc"`
	NeedsGPU     bool     `json:"needs_gpu"`
	MinCPUCores  int      `json:"min_cpu_cores"`
	MinRAMMb     int      `json:"min_ram_mb"`
	MinGPUVRAMMb int      `json:"min_gpu_vram_mb"`
	GPUCount     int      `json:"gpu_count"`
	HeavyDeps    []string `json:"heavy_deps"`
	PreferredBase string  `json:"preferred_base"`
	InfraDeps    []string `json:"infra_deps"`
	DeployMode   string   `json:"deploy_mode"`
}

// ParseRequirements parses a RuntimeRequirements JSON string (from repo_memory).
func ParseRequirements(jsonStr string) *RuntimeRequirements {
	if jsonStr == "" {
		return nil
	}
	var req RuntimeRequirements
	if err := json.Unmarshal([]byte(jsonStr), &req); err != nil {
		slog.Warn("failed to parse runtime_requirements", "error", err)
		return nil
	}
	return &req
}

// ImageSelection is the result of comparing project requirements against the host.
type ImageSelection struct {
	Image       string `json:"image"`        // Selected runner image name
	CPUReq      string `json:"cpu_req"`      // K8s CPU request
	CPULim      string `json:"cpu_lim"`      // K8s CPU limit
	MemReq      string `json:"mem_req"`      // K8s memory request
	MemLim      string `json:"mem_lim"`      // K8s memory limit
	Feasible    bool   `json:"feasible"`     // Whether the host can support this project
	Reason      string `json:"reason"`       // Human-readable explanation
}

// SelectImage picks the best runner image and resource limits for a project,
// given its requirements and the host profile. Returns feasibility info.
func SelectImage(req *RuntimeRequirements, host *HostProfile, baseImage string) ImageSelection {
	sel := ImageSelection{
		Image:    baseImage,
		CPUReq:   "200m",
		CPULim:   "500m",
		MemReq:   "512Mi",
		MemLim:   "1Gi",
		Feasible: true,
	}

	if req == nil {
		sel.Reason = "No runtime requirements detected; using default image"
		return sel
	}

	// --- Image selection ---

	switch strings.ToLower(req.Language) {
	case "python":
		if req.NeedsGLibC || len(req.HeavyDeps) > 0 {
			sel.Image = registryPrefix(baseImage) + ImagePython
			sel.Reason = fmt.Sprintf("Python project with C extension deps %v; using debian-slim runner", req.HeavyDeps)
		} else {
			// Simple Python project — alpine is fine
			sel.Reason = "Python project with no C extensions; default runner is fine"
		}
	case "go", "rust":
		sel.Reason = fmt.Sprintf("%s project; static binary — alpine runner is fine", req.Language)
	case "javascript", "typescript":
		sel.Reason = fmt.Sprintf("%s project; default runner is fine", req.Language)
	default:
		if req.NeedsGLibC {
			sel.Image = registryPrefix(baseImage) + ImagePython
			sel.Reason = fmt.Sprintf("Project needs glibc; using debian-slim runner")
		} else {
			sel.Reason = fmt.Sprintf("Language=%s; using default runner", req.Language)
		}
	}

	// --- Resource sizing ---

	if req.MinCPUCores > 0 {
		cpuMilli := req.MinCPUCores * 1000
		sel.CPUReq = fmt.Sprintf("%dm", cpuMilli/2) // request half of minimum
		sel.CPULim = fmt.Sprintf("%dm", cpuMilli)
	}
	if req.MinRAMMb > 0 {
		sel.MemReq = fmt.Sprintf("%dMi", req.MinRAMMb)
		sel.MemLim = fmt.Sprintf("%dMi", req.MinRAMMb*2)
	}

	// --- Feasibility checks ---

	if req.NeedsGPU && host != nil {
		if !host.HasGPU() {
			sel.Feasible = false
			sel.Reason = formatInfeasible(req, host)
			return sel
		}
		if req.MinGPUVRAMMb > 0 && host.TotalGPUVRAM > 0 && req.MinGPUVRAMMb > host.TotalGPUVRAM {
			sel.Feasible = false
			sel.Reason = formatInfeasible(req, host)
			return sel
		}
		if req.GPUCount > 0 && len(host.GPUs) < req.GPUCount {
			sel.Feasible = false
			sel.Reason = formatInfeasible(req, host)
			return sel
		}
	}

	if host != nil {
		if req.MinRAMMb > 0 && host.RAMMb > 0 && req.MinRAMMb > host.RAMMb {
			sel.Feasible = false
			sel.Reason = formatInfeasible(req, host)
			return sel
		}
		if req.MinCPUCores > 0 && host.CPUCores > 0 && req.MinCPUCores > host.CPUCores {
			sel.Feasible = false
			sel.Reason = formatInfeasible(req, host)
			return sel
		}
	}

	return sel
}

// formatInfeasible builds a detailed message about why the host can't support the project.
func formatInfeasible(req *RuntimeRequirements, host *HostProfile) string {
	var parts []string

	if req.NeedsGPU {
		if !host.HasGPU() {
			gpuDesc := "GPU"
			if req.GPUCount > 1 {
				gpuDesc = fmt.Sprintf("%dx GPU", req.GPUCount)
			}
			if req.MinGPUVRAMMb > 0 {
				gpuDesc += fmt.Sprintf(" (%dMB VRAM)", req.MinGPUVRAMMb)
			}
			parts = append(parts, fmt.Sprintf("requires %s; host has no GPU", gpuDesc))
		} else if req.MinGPUVRAMMb > 0 && host.TotalGPUVRAM > 0 && req.MinGPUVRAMMb > host.TotalGPUVRAM {
			parts = append(parts, fmt.Sprintf("requires %dMB GPU VRAM; host has %dMB", req.MinGPUVRAMMb, host.TotalGPUVRAM))
		} else if req.GPUCount > 0 && len(host.GPUs) < req.GPUCount {
			parts = append(parts, fmt.Sprintf("requires %d GPUs; host has %d", req.GPUCount, len(host.GPUs)))
		}
	}
	if req.MinRAMMb > 0 && host.RAMMb > 0 && req.MinRAMMb > host.RAMMb {
		parts = append(parts, fmt.Sprintf("requires %dMB RAM; host has %dMB", req.MinRAMMb, host.RAMMb))
	}
	if req.MinCPUCores > 0 && host.CPUCores > 0 && req.MinCPUCores > host.CPUCores {
		parts = append(parts, fmt.Sprintf("requires %d CPU cores; host has %d", req.MinCPUCores, host.CPUCores))
	}

	if len(parts) == 0 {
		return "host cannot support project requirements"
	}
	return "Full reproduction infeasible: " + strings.Join(parts, "; ") + ". Performing code analysis only."
}

// registryPrefix extracts the registry prefix from a base image name.
// e.g. "registry.example.com/ns/opinai-controller:latest" -> "registry.example.com/ns/"
// e.g. "opinai-controller:latest" -> "" (local image, no prefix)
func registryPrefix(baseImage string) string {
	// Strip tag
	img := baseImage
	if idx := strings.LastIndex(img, ":"); idx > 0 {
		img = img[:idx]
	}
	// Find the last slash — everything before it is the prefix
	if idx := strings.LastIndex(img, "/"); idx > 0 {
		return img[:idx+1]
	}
	return ""
}
