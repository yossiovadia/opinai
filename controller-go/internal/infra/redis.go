package infra

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	redisName = "opinai-redis"
	redisPort = 6379
)

// RedisProvider manages a single-instance Redis via Deployment.
type RedisProvider struct{}

func (p *RedisProvider) Install(ctx context.Context, client kubernetes.Interface, namespace string) (string, error) {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisName,
			Namespace: namespace,
			Labels:    infraLabels(redisName),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(redisName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(redisName)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "redis",
						Image: "redis:7-alpine",
						Ports: []corev1.ContainerPort{{ContainerPort: int32(redisPort), Name: "redis"}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"redis-cli", "ping"},
								},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       3,
						},
					}},
				},
			},
		},
	}
	if _, err := client.AppsV1().Deployments(namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create Deployment: %w", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisName,
			Namespace: namespace,
			Labels:    infraLabels(redisName),
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels(redisName),
			Ports: []corev1.ServicePort{{
				Name: "redis",
				Port: int32(redisPort),
			}},
		},
	}
	if _, err := client.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create Service: %w", err)
	}

	if err := waitForReady(ctx, client, namespace, selectorLabels(redisName), 2*time.Minute); err != nil {
		return "", fmt.Errorf("wait for Redis ready: %w", err)
	}

	connInfo := p.ConnectionInfo(namespace)
	slog.Info("Redis installed and ready", "connection", connInfo)
	return connInfo, nil
}

func (p *RedisProvider) Start(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleDeployment(ctx, client, namespace, redisName, 1)
}

func (p *RedisProvider) Stop(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleDeployment(ctx, client, namespace, redisName, 0)
}

func (p *RedisProvider) Teardown(ctx context.Context, client kubernetes.Interface, namespace string) error {
	bg := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &bg}
	client.AppsV1().Deployments(namespace).Delete(ctx, redisName, opts)
	client.CoreV1().Services(namespace).Delete(ctx, redisName, opts)
	return nil
}

func (p *RedisProvider) IsRunning(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	return hasReadyPods(ctx, client, namespace, selectorLabels(redisName))
}

func (p *RedisProvider) ConnectionInfo(namespace string) string {
	return fmt.Sprintf("redis://%s.%s.svc:%d", redisName, namespace, redisPort)
}

func (p *RedisProvider) EnvVarName() string {
	return "OPINAI_INFRA_REDIS"
}
