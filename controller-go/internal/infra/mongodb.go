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
	mongoName     = "opinai-mongodb"
	mongoUser     = "opinai"
	mongoPassword = "opinai"
	mongoDB       = "opinai"
	mongoPort     = 27017
)

// MongoDBProvider manages a single-instance MongoDB via StatefulSet with persistent storage.
type MongoDBProvider struct{}

func (p *MongoDBProvider) Install(ctx context.Context, client kubernetes.Interface, namespace string) (string, error) {
	// Create PVC
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mongoName + "-data",
			Namespace: namespace,
			Labels:    infraLabels(mongoName),
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

	replicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mongoName,
			Namespace: namespace,
			Labels:    infraLabels(mongoName),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: mongoName,
			Selector:    &metav1.LabelSelector{MatchLabels: selectorLabels(mongoName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(mongoName)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "mongodb",
						Image: "mongo:7",
						Ports: []corev1.ContainerPort{{ContainerPort: int32(mongoPort), Name: "mongodb"}},
						Env: []corev1.EnvVar{
							{Name: "MONGO_INITDB_ROOT_USERNAME", Value: mongoUser},
							{Name: "MONGO_INITDB_ROOT_PASSWORD", Value: mongoPassword},
							{Name: "MONGO_INITDB_DATABASE", Value: mongoDB},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data/db",
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
									Command: []string{"mongosh", "--eval", "db.adminCommand('ping')"},
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
								ClaimName: mongoName + "-data",
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

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mongoName,
			Namespace: namespace,
			Labels:    infraLabels(mongoName),
		},
		Spec: corev1.ServiceSpec{
			Selector:  selectorLabels(mongoName),
			ClusterIP: "None",
			Ports: []corev1.ServicePort{{
				Name: "mongodb",
				Port: int32(mongoPort),
			}},
		},
	}
	if _, err := client.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create Service: %w", err)
	}

	if err := waitForReady(ctx, client, namespace, selectorLabels(mongoName), 3*time.Minute); err != nil {
		return "", fmt.Errorf("wait for MongoDB ready: %w", err)
	}

	connInfo := p.ConnectionInfo(namespace)
	slog.Info("MongoDB installed and ready", "connection", connInfo)
	return connInfo, nil
}

func (p *MongoDBProvider) Start(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleStatefulSet(ctx, client, namespace, mongoName, 1)
}

func (p *MongoDBProvider) Stop(ctx context.Context, client kubernetes.Interface, namespace string) error {
	return scaleStatefulSet(ctx, client, namespace, mongoName, 0)
}

func (p *MongoDBProvider) Teardown(ctx context.Context, client kubernetes.Interface, namespace string) error {
	bg := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &bg}
	client.AppsV1().StatefulSets(namespace).Delete(ctx, mongoName, opts)
	client.CoreV1().Services(namespace).Delete(ctx, mongoName, opts)
	client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, mongoName+"-data", opts)
	return nil
}

func (p *MongoDBProvider) IsRunning(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error) {
	return hasReadyPods(ctx, client, namespace, selectorLabels(mongoName))
}

func (p *MongoDBProvider) ConnectionInfo(namespace string) string {
	return fmt.Sprintf("mongodb://%s:%s@%s.%s.svc:%d/%s?authSource=admin",
		mongoUser, mongoPassword, mongoName, namespace, mongoPort, mongoDB)
}

func (p *MongoDBProvider) EnvVarName() string {
	return "OPINAI_INFRA_MONGODB"
}
