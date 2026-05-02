// Package k8sproof validates Kubernetes projected service account tokens and
// verifies pod provenance before allowing a CI job claim.
//
// The validation chain is:
//  1. TokenReview — token must be valid and issued for the ci-worker service account
//  2. Pod name and UID extracted from token extra claims
//  3. Pod fetched; UID cross-checked against token claim
//  4. Pod freshness — pod.creationTimestamp must be < maxAge ago
//  5. Pod phase — pod must be in Running phase
//  6. Container image digest — every container image must reference a digest (@sha256:)
//  7. Owner chain — pod → batch/v1 Job → keda.sh/v1alpha1 ScaledJob named "ci-worker"
package k8sproof

import (
	"context"
	"fmt"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// expectedServiceAccount is the fully-qualified K8s service account name
	// that ci-worker pods must run as.
	expectedServiceAccount = "system:serviceaccount:docstore-ci:ci-worker"

	// DefaultMaxPodAge is the default maximum age for a pod to be eligible to claim a job.
	// Set to 4 hours to accommodate warm pool workers (KEDA ScaledJob minReplicaCount > 0).
	DefaultMaxPodAge = 4 * time.Hour

	// expectedScaledJobName is the KEDA ScaledJob that must own the pod.
	expectedScaledJobName = "ci-worker"
)

// PodClaimer validates a Kubernetes projected service account token and
// verifies the pod's provenance before allowing a CI job claim.
type PodClaimer struct {
	client    kubernetes.Interface
	namespace string
	maxAge    time.Duration
}

// New creates a PodClaimer that validates tokens against the K8s API server.
// namespace is the Kubernetes namespace where ci-worker pods run.
func New(client kubernetes.Interface, namespace string) *PodClaimer {
	return &PodClaimer{
		client:    client,
		namespace: namespace,
		maxAge:    DefaultMaxPodAge,
	}
}

// ValidateToken validates a Kubernetes projected service account token and
// verifies the calling pod's provenance. It returns the pod name and pod IP
// on success. On failure it returns a non-nil error describing which check
// failed.
func (p *PodClaimer) ValidateToken(ctx context.Context, token string) (podName, podIP string, err error) {
	// 1. TokenReview — validate the SA token via the K8s API.
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: token,
		},
	}
	result, err := p.client.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("token review failed: %w", err)
	}
	if !result.Status.Authenticated {
		return "", "", fmt.Errorf("token not authenticated: %s", result.Status.Error)
	}
	if result.Status.User.Username != expectedServiceAccount {
		return "", "", fmt.Errorf("unexpected service account %q (want %q)",
			result.Status.User.Username, expectedServiceAccount)
	}

	// 2. Extract pod name and UID from extra claims.
	podNames := result.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	if len(podNames) == 0 {
		return "", "", fmt.Errorf("pod name not present in token extra claims")
	}
	podName = string(podNames[0])

	podUIDs := result.Status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if len(podUIDs) == 0 {
		return "", "", fmt.Errorf("pod UID not present in token extra claims")
	}
	tokenPodUID := string(podUIDs[0])

	// 3. Fetch the pod and cross-check the UID.
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("fetch pod %q: %w", podName, err)
	}
	if string(pod.UID) != tokenPodUID {
		return "", "", fmt.Errorf("pod UID mismatch: token claims %q, pod has %q", tokenPodUID, pod.UID)
	}

	// 4. Check pod freshness.
	podAge := time.Since(pod.CreationTimestamp.Time)
	if podAge > p.maxAge {
		return "", "", fmt.Errorf("pod %q is too old (%s > %s)", podName, podAge.Round(time.Second), p.maxAge)
	}
	podIP = pod.Status.PodIP

	// 5. Pod must be in Running phase.
	if pod.Status.Phase != corev1.PodRunning {
		return "", "", fmt.Errorf("pod %q is not running (phase: %s)", podName, pod.Status.Phase)
	}

	// 6. Every container image must be digest-pinned.
	for _, c := range pod.Spec.Containers {
		if !strings.Contains(c.Image, "@sha256:") {
			return "", "", fmt.Errorf("container %q image %q is not digest-pinned (missing @sha256:)", c.Name, c.Image)
		}
	}

	// 7. Pod must be owned by a batch/v1 Job.
	var jobName string
	for _, ref := range pod.OwnerReferences {
		if ref.APIVersion == "batch/v1" && ref.Kind == "Job" {
			jobName = ref.Name
			break
		}
	}
	if jobName == "" {
		return "", "", fmt.Errorf("pod %q has no batch/v1 Job owner", podName)
	}

	// 8. That Job must be owned by the ci-worker KEDA ScaledJob.
	job, err := p.client.BatchV1().Jobs(p.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("fetch job %q: %w", jobName, err)
	}
	var scaledJobName string
	for _, ref := range job.OwnerReferences {
		if ref.APIVersion == "keda.sh/v1alpha1" && ref.Kind == "ScaledJob" {
			scaledJobName = ref.Name
			break
		}
	}
	if scaledJobName == "" {
		return "", "", fmt.Errorf("job %q has no keda.sh/v1alpha1 ScaledJob owner", jobName)
	}
	if scaledJobName != expectedScaledJobName {
		return "", "", fmt.Errorf("job %q owned by ScaledJob %q (want %q)",
			jobName, scaledJobName, expectedScaledJobName)
	}

	return podName, podIP, nil
}
