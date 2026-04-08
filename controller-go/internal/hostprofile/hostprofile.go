// Package hostprofile detects the runtime environment (K8s cluster or local machine)
// and exposes a HostProfile used for runner image selection and feasibility checks.
package hostprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GPU describes a single GPU device.
type GPU struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	VRAMMb int   `json:"vram_mb"`
}

// HostProfile describes the runtime environment OpinAI is deployed on.
type HostProfile struct {
	// Detection mode
	Mode string `json:"mode"` // "kubernetes", "openshift", "local"

	// Compute
	CPUCores     int    `json:"cpu_cores"`
	Arch         string `json:"arch"` // amd64, arm64
	RAMMb        int    `json:"ram_mb"`
	GPUs         []GPU  `json:"gpus,omitempty"`
	TotalGPUVRAM int    `json:"total_gpu_vram_mb"`

	// OS/Runtime
	OS            string   `json:"os"`             // linux, darwin
	Distro        string   `json:"distro"`         // alpine, debian, ubuntu, etc.
	LibC          string   `json:"libc"`           // musl, glibc
	Runtimes      []string `json:"runtimes"`       // ["python3.12", "go1.25", "node20"]

	// K8s-specific (empty for local mode)
	K8sVersion       string   `json:"k8s_version,omitempty"`
	ContainerRuntime string   `json:"container_runtime,omitempty"`
	StorageClasses   []string `json:"storage_classes,omitempty"`
	InstalledCRDs    []string `json:"installed_crds,omitempty"`
	InstalledOperators []string `json:"installed_operators,omitempty"`
	NodeLabels       map[string]string `json:"node_labels,omitempty"`
	IsOpenShift      bool   `json:"is_openshift"`
	AllocatableCPU   string `json:"allocatable_cpu,omitempty"`   // e.g. "8" or "4000m"
	AllocatableRAM   string `json:"allocatable_ram,omitempty"`   // e.g. "16Gi"
	AllocatableGPU   int    `json:"allocatable_gpu,omitempty"`   // nvidia.com/gpu total

	// Metadata
	DetectedAt string `json:"detected_at"`
}

// Detect builds a HostProfile by probing the current environment.
// If a K8s client is provided, it queries the cluster; otherwise it detects locally.
func Detect(k8sClient kubernetes.Interface) *HostProfile {
	p := &HostProfile{
		Arch:       runtime.GOARCH,
		OS:         runtime.GOOS,
		CPUCores:   runtime.NumCPU(),
		DetectedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	// Local detection (always runs)
	p.RAMMb = detectRAM()
	p.Distro, p.LibC = detectDistroLibC()
	p.Runtimes = detectRuntimes()
	p.GPUs, p.TotalGPUVRAM = detectGPUs()

	// K8s detection
	if k8sClient != nil {
		p.detectK8s(k8sClient)
	} else {
		p.Mode = "local"
	}

	return p
}

// JSON returns the profile as a JSON string.
func (p *HostProfile) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// Summary returns a human-readable one-liner for log output.
func (p *HostProfile) Summary() string {
	gpu := "no GPU"
	if len(p.GPUs) > 0 {
		names := make([]string, len(p.GPUs))
		for i, g := range p.GPUs {
			names[i] = g.Name
		}
		gpu = fmt.Sprintf("%dx GPU (%s, %dMB VRAM)", len(p.GPUs), strings.Join(names, ", "), p.TotalGPUVRAM)
	}
	return fmt.Sprintf("mode=%s arch=%s cpu=%d ram=%dMB %s libc=%s runtimes=%v",
		p.Mode, p.Arch, p.CPUCores, p.RAMMb, gpu, p.LibC, p.Runtimes)
}

// HasGPU returns true if at least one GPU is available.
func (p *HostProfile) HasGPU() bool {
	return len(p.GPUs) > 0
}

// HasGLibC returns true if the host uses glibc (not musl).
func (p *HostProfile) HasGLibC() bool {
	return p.LibC == "glibc"
}

// --- K8s detection ---

func (p *HostProfile) detectK8s(client kubernetes.Interface) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Detect OpenShift vs vanilla K8s
	p.IsOpenShift = detectOpenShift(client)
	if p.IsOpenShift {
		p.Mode = "openshift"
	} else {
		p.Mode = "kubernetes"
	}

	// Server version
	if ver, err := client.Discovery().ServerVersion(); err == nil {
		p.K8sVersion = ver.GitVersion
	}

	// Node resources (sum allocatable across all nodes)
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil {
		totalCPU := int64(0)
		totalRAM := int64(0)
		totalGPU := int64(0)
		labels := map[string]string{}

		for _, node := range nodes.Items {
			alloc := node.Status.Allocatable
			if cpu, ok := alloc["cpu"]; ok {
				totalCPU += cpu.MilliValue()
			}
			if mem, ok := alloc["memory"]; ok {
				totalRAM += mem.Value() / (1024 * 1024) // bytes -> MB
			}
			if gpu, ok := alloc["nvidia.com/gpu"]; ok {
				totalGPU += gpu.Value()
			}
			// Collect interesting labels from all nodes
			for k, v := range node.Labels {
				if isInterestingLabel(k) {
					labels[k] = v
				}
			}
		}

		p.AllocatableCPU = fmt.Sprintf("%dm", totalCPU)
		p.AllocatableRAM = fmt.Sprintf("%dMi", totalRAM)
		p.AllocatableGPU = int(totalGPU)
		p.CPUCores = int(totalCPU / 1000)
		p.RAMMb = int(totalRAM)
		if len(labels) > 0 {
			p.NodeLabels = labels
		}
		if totalGPU > 0 {
			p.GPUs = []GPU{{Index: 0, Name: "nvidia (cluster)", VRAMMb: 0}}
			p.TotalGPUVRAM = 0 // K8s doesn't report VRAM
		}
	}

	// Storage classes
	scList, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, sc := range scList.Items {
			p.StorageClasses = append(p.StorageClasses, sc.Name)
		}
	}

	// Container runtime (from first node)
	if nodes != nil && len(nodes.Items) > 0 {
		p.ContainerRuntime = nodes.Items[0].Status.NodeInfo.ContainerRuntimeVersion
	}

	// CRDs (via discovery)
	p.InstalledCRDs = detectCRDs(client)

	// Operators (OLM subscriptions) - best effort
	p.InstalledOperators = detectOperators(client)
}

func detectOpenShift(client kubernetes.Interface) bool {
	_, resources, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		return false
	}
	for _, rl := range resources {
		if strings.HasPrefix(rl.GroupVersion, "route.openshift.io") {
			return true
		}
	}
	return false
}

func detectCRDs(client kubernetes.Interface) []string {
	_, resources, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		return nil
	}
	var crds []string
	seen := map[string]bool{}
	for _, rl := range resources {
		// Skip core API groups
		gv := rl.GroupVersion
		if !strings.Contains(gv, ".") {
			continue
		}
		group := strings.SplitN(gv, "/", 2)[0]
		if seen[group] {
			continue
		}
		// Skip well-known K8s groups
		if isBuiltinGroup(group) {
			continue
		}
		seen[group] = true
		crds = append(crds, group)
	}
	return crds
}

func detectOperators(client kubernetes.Interface) []string {
	// OLM subscriptions are in operators.coreos.com - check via discovery
	_, resources, err := client.Discovery().ServerGroupsAndResources()
	if err != nil {
		return nil
	}
	for _, rl := range resources {
		if strings.HasPrefix(rl.GroupVersion, "operators.coreos.com") {
			// OLM is installed; we'd need a dynamic client to list subscriptions.
			// For now, just note that OLM is present.
			return []string{"OLM-installed"}
		}
	}
	return nil
}

func isInterestingLabel(key string) bool {
	interesting := []string{
		"nvidia.com/gpu",
		"beta.kubernetes.io/arch",
		"kubernetes.io/arch",
		"node.kubernetes.io/instance-type",
		"topology.kubernetes.io/zone",
		"feature.node.kubernetes.io/cpu",
		"accelerator",
	}
	for _, prefix := range interesting {
		if strings.HasPrefix(key, prefix) || strings.Contains(key, "gpu") {
			return true
		}
	}
	return false
}

func isBuiltinGroup(group string) bool {
	builtins := []string{
		"apps", "batch", "autoscaling", "policy", "networking.k8s.io",
		"rbac.authorization.k8s.io", "storage.k8s.io", "admissionregistration.k8s.io",
		"apiextensions.k8s.io", "apiregistration.k8s.io", "authentication.k8s.io",
		"authorization.k8s.io", "certificates.k8s.io", "coordination.k8s.io",
		"discovery.k8s.io", "events.k8s.io", "flowcontrol.apiserver.k8s.io",
		"node.k8s.io", "scheduling.k8s.io", "snapshot.storage.k8s.io",
	}
	for _, b := range builtins {
		if group == b {
			return true
		}
	}
	return false
}

// --- Local detection ---

func detectRAM() int {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.Atoi(fields[1])
					return kb / 1024
				}
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			bytes, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			return int(bytes / (1024 * 1024))
		}
	}
	return 0
}

func detectDistroLibC() (distro, libc string) {
	// Check /etc/os-release
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "ID=") {
				distro = strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
			}
		}
	}

	if runtime.GOOS == "darwin" {
		return "macos", "glibc"
	}

	// Detect libc
	if _, err := os.Stat("/lib/ld-musl-x86_64.so.1"); err == nil {
		return distro, "musl"
	}
	if _, err := os.Stat("/lib/ld-musl-aarch64.so.1"); err == nil {
		return distro, "musl"
	}
	// Check ldd output
	out, err := exec.Command("ldd", "--version").CombinedOutput()
	if err == nil {
		s := strings.ToLower(string(out))
		if strings.Contains(s, "musl") {
			return distro, "musl"
		}
	}
	return distro, "glibc"
}

func detectRuntimes() []string {
	var runtimes []string
	checks := []struct {
		name string
		cmd  string
		args []string
		parse func(string) string
	}{
		{"python", "python3", []string{"--version"}, func(s string) string {
			// "Python 3.12.1" -> "python3.12"
			s = strings.TrimSpace(s)
			parts := strings.Fields(s)
			if len(parts) >= 2 {
				ver := parts[1]
				dotParts := strings.SplitN(ver, ".", 3)
				if len(dotParts) >= 2 {
					return "python" + dotParts[0] + "." + dotParts[1]
				}
			}
			return ""
		}},
		{"go", "go", []string{"version"}, func(s string) string {
			// "go version go1.25 linux/amd64" -> "go1.25"
			for _, field := range strings.Fields(s) {
				if strings.HasPrefix(field, "go1.") {
					return field
				}
			}
			return ""
		}},
		{"node", "node", []string{"--version"}, func(s string) string {
			// "v20.11.0" -> "node20"
			s = strings.TrimSpace(s)
			s = strings.TrimPrefix(s, "v")
			parts := strings.SplitN(s, ".", 3)
			if len(parts) >= 1 {
				return "node" + parts[0]
			}
			return ""
		}},
	}

	for _, c := range checks {
		out, err := exec.Command(c.cmd, c.args...).CombinedOutput()
		if err != nil {
			continue
		}
		if r := c.parse(string(out)); r != "" {
			runtimes = append(runtimes, r)
		}
	}
	return runtimes
}

func detectGPUs() ([]GPU, int) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total",
		"--format=csv,noheader,nounits").CombinedOutput()
	if err != nil {
		return nil, 0
	}

	var gpus []GPU
	totalVRAM := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ", ", 3)
		if len(parts) < 3 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		name := strings.TrimSpace(parts[1])
		vram, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		gpus = append(gpus, GPU{Index: idx, Name: name, VRAMMb: vram})
		totalVRAM += vram
	}
	return gpus, totalVRAM
}

// LogSummary logs the host profile summary at startup.
func LogSummary(p *HostProfile) {
	slog.Info("host profile detected", "summary", p.Summary())
	if p.Mode != "local" {
		slog.Info("cluster info",
			"k8s_version", p.K8sVersion,
			"openshift", p.IsOpenShift,
			"allocatable_cpu", p.AllocatableCPU,
			"allocatable_ram", p.AllocatableRAM,
			"allocatable_gpu", p.AllocatableGPU,
			"storage_classes", len(p.StorageClasses),
			"crds", len(p.InstalledCRDs),
		)
	}
	if len(p.GPUs) > 0 {
		for _, g := range p.GPUs {
			slog.Info("GPU detected", "index", g.Index, "name", g.Name, "vram_mb", g.VRAMMb)
		}
	}
}
