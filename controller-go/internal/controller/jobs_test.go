package controller

import (
	"testing"
)

func TestExtractPlanResources(t *testing.T) {
	// Empty plan
	r := extractPlanResources("")
	if r.CPUReq != "200m" || r.MemReq != "512Mi" || r.TimeoutMinutes != 10 {
		t.Errorf("empty defaults: %+v", r)
	}

	// Full plan
	plan := `{"install_command":"pip install --user foo","resource_requirements":{"cpu":"500m","memory":"1Gi"},"job_timeout_minutes":20}`
	r = extractPlanResources(plan)
	if r.InstallCommand != "pip install --user foo" {
		t.Errorf("install = %q", r.InstallCommand)
	}
	if r.CPUReq != "500m" {
		t.Errorf("cpu = %q", r.CPUReq)
	}
	if r.MemReq != "1Gi" {
		t.Errorf("mem = %q", r.MemReq)
	}
	if r.TimeoutMinutes != 20 {
		t.Errorf("timeout = %d", r.TimeoutMinutes)
	}

	// Partial plan (no resources)
	r = extractPlanResources(`{"install_command":"make build"}`)
	if r.InstallCommand != "make build" {
		t.Errorf("install = %q", r.InstallCommand)
	}
	if r.CPUReq != "200m" {
		t.Errorf("partial should keep defaults: %+v", r)
	}

	// Invalid JSON
	r = extractPlanResources("not json")
	if r.CPUReq != "200m" {
		t.Errorf("invalid should use defaults: %+v", r)
	}
}

