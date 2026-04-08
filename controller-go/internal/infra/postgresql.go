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
	pgName     = "opinai-postgresql"
	pgUser     = "opinai"
	pgPassword = "opinai"
	pgDB       = "opinai"
	pgPort     = 5432
)

// PostgreSQLProvider manages a single-instance PostgreSQL via StatefulSet.
type PostgreSQLProvider struct{}

func (p *PostgreSQLProvider) Install(ctx context.Context, client kubernetes.Interface, namespace string) (string, error) {
	// Create PVC for data persistence
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgName + "-data",
			Namespace: namespace,
			Labels:    infraLabels(pgName),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("5Gi"),
				},
			},
		},
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create PVC: %w", err)
	}

	// Create StatefulSet
	replicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgName,
			Namespace: namespace,
			Labels:    infraLabels(pgName),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: pgName,
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(pgName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(pgName)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "postgresql",
						Image: "postgres:16-alpine",
						Ports: []corev1.ContainerPort{{ContainerPort: int32(pgPort), Name: "postgresql"}},
						Env: []corev1.EnvVar{
							{Name: "POSTGRES_USER", Value: pgUser},
							{Name: "POSTGRES_PASSWORD", Value: pgPassword},
							{Name: "POSTGRES_DB", Value: pgDB},
							{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/var/lib/postgresql/data",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"pg_isready", "-U", pgUser},
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pgName + "-data",
							},
						},
					}},
				},
			},
		},
	}
	if _, err := client.AppsV1().StatefulSets(namespace).Create(ctx, sts, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create StatefulSet: %w", err)
	}

	// Create headless Service for stable DNS
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgName,
			Namespace: namespace,
			Labels:    infraLabels(pgName),
		},
		Spec: corev1.ServiceSpec{
			Selector:  selectorLabels(pgName),
			ClusterIP: "None",
			Ports: []corev1.ServicePort{{
				Name: "postgresql",
				Port: int32(pgPort),
			}},
		},
	}
	if _, err := client.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create Service: %w", err)
	}

	// Wait for pod readiness
	if err := waitForReady(ctx, client, namespace, selectorLabels(pgName), 3*time.Minute); err != nil {
		return "", fmt.Errorf("wait for PostgreSQL ready: %w", err)
	}

	connInfo := p.ConnectionInfo(namespace)
	slog.Info("PostgreSQL installed and ready", "connection", connInfo)
	return connInfo, nil
}

func (p *PostgreSQLProvider) Start(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleStatefulSet(ctx, client, namespace, pgName, 1)
}

func (p *PostgreSQLProvider) Stop(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleStatefulSet(ctx, client, namespace, pgName, 0)
}

func (p *PostgreSQLProvider) Teardown(ctx context.Context, client kubernetes.Interface, namespace string) error {
	bg := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &bg}
	// Delete in order: StatefulSet, Service, PVC
	client.AppsV1().StatefulSets(namespace).Delete(ctx, pgName, opts)
	client.CoreV1().Services(namespace).Delete(ctx, pgName, opts)
	client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pgName+"-data", opts)
	return nil
}

func (p *PostgreSQLProvider) IsRunning(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	return hasReadyPods(ctx, client, namespace, selectorLabels(pgName))
}

func (p *PostgreSQLProvider) ConnectionInfo(namespace string) string {
	return fmt.Sprintf("postgresql://%s:%s@%s.%s.svc:%d/%s?sslmode=disable",
		pgUser, pgPassword, pgName, namespace, pgPort, pgDB)
}

func (p *PostgreSQLProvider) EnvVarName() string {
	return "OPINAI_INFRA_POSTGRESQL"
}
