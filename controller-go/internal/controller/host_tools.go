package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var hostToolRepos = []string{
	"opendatahub-io/models-as-a-service",
	"opendatahub-io/ai-gateway-payload-processing",
}

func repoNeedsHostTools(repo string) bool {
	lower := strings.ToLower(repo)
	for _, r := range hostToolRepos {
		if strings.ToLower(r) == lower {
			return true
		}
	}
	return false
}

func hostToolVolumes(needsHostTools bool) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name: "gcp-credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "opinai-gcp-credentials",
					Optional:   boolPtr(true),
				},
			},
		},
	}
	if needsHostTools {
		hostPathSocket := corev1.HostPathSocket
		vols = append(vols, corev1.Volume{
			Name: "docker-socket",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/docker.sock",
					Type: &hostPathSocket,
				},
			},
		})
	}
	return vols
}

func hostToolVolumeMounts(needsHostTools bool) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "gcp-credentials", MountPath: "/var/run/secrets/gcp", ReadOnly: true},
	}
	if needsHostTools {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "docker-socket",
			MountPath: "/var/run/docker.sock",
		})
	}
	return mounts
}

func hostToolEnvVars(needsHostTools bool) []corev1.EnvVar {
	if !needsHostTools {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "OPINAI_HOST_TOOLS", Value: "true"},
		{Name: "OPINAI_KIND_CLUSTER", Value: "maas-local"},
		{Name: "OPINAI_LOCAL_DEPLOY_SCRIPT", Value: "test/e2e/scripts/local-deploy.sh"},
	}
}
