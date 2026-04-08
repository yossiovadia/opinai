package infra

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// infraLabels returns the standard labels for an infra resource.
func infraLabels(name string) map[string]string {
	return map[string]string{
		"app":                name,
		"opinai.dev/managed": "true",
		"opinai.dev/role":    "infrastructure",
	}
}

// selectorLabels returns the minimal labels for pod selection.
func selectorLabels(name string) map[string]string {
	return map[string]string{
		"app": name,
	}
}

// scaleStatefulSet sets the replica count for a StatefulSet.
func scaleStatefulSet(ctx context.Context, client kubernetes.Interface, namespace, name string, replicas int32) error {
	sts, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get StatefulSet %s: %w", name, err)
	}
	sts.Spec.Replicas = &replicas
	if _, err := client.AppsV1().StatefulSets(namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale StatefulSet %s to %d: %w", name, replicas, err)
	}
	if replicas > 0 {
		return waitForReady(ctx, client, namespace, selectorLabels(name), 3*time.Minute)
	}
	return nil
}

// scaleDeployment sets the replica count for a Deployment.
func scaleDeployment(ctx context.Context, client kubernetes.Interface, namespace, name string, replicas int32) error {
	dep, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Deployment %s: %w", name, err)
	}
	dep.Spec.Replicas = &replicas
	if _, err := client.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale Deployment %s to %d: %w", name, replicas, err)
	}
	if replicas > 0 {
		return waitForReady(ctx, client, namespace, selectorLabels(name), 3*time.Minute)
	}
	return nil
}

// hasReadyPods returns true if at least one pod matching the labels is Ready.
func hasReadyPods(ctx context.Context, client kubernetes.Interface, namespace string, labelSet map[string]string) (bool, error) {
	sel := labels.SelectorFromSet(labelSet).String()
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return true, nil
			}
		}
	}
	return false, nil
}

// waitForReady polls until at least one matching pod is Ready or the context times out.
func waitForReady(ctx context.Context, client kubernetes.Interface, namespace string, labelSet map[string]string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.Done():
			return fmt.Errorf("timed out waiting for pods with labels %v to become ready", labelSet)
		case <-ticker.C:
			ready, err := hasReadyPods(deadline, client, namespace, labelSet)
			if err != nil {
				continue
			}
			if ready {
				return nil
			}
		}
	}
}

