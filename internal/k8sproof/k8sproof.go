// Package k8sproof validates Kubernetes projected service account tokens and
// verifies pod provenance before allowing a CI job claim.
//
// The validation chain is:
//  1. TokenReview — token must be valid and issued for the ci-worker service account
//  2. Pod freshness — pod.creationTimestamp must be < maxAge ago
//  3. Owner chain — pod → batch/v1 Job → keda.sh/v1alpha1 ScaledJob named "ci-worker"
package k8sproof

import (
	"context"
	"fmt"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// expectedServiceAccount is the fully-qualified K8s service account name
	// that ci-worker pods must run as.
	expectedServiceAccount = "system:serviceaccount:docstore-ci:ci-worker"

	// maxPodAge is the maximum age of a pod that may claim a job.
	// This prevents long-lived or recycled pods from claiming jobs.
	maxPodAge = 10 * time.Minute

	// expectedScaledJobName is the KEDA ScaledJob that must own the pod.
	expectedScaledJobName = "ci-worker"
)

// PodClaimer validates a Kubernetes projected service account token and
// verifies the pod's provenance before allowing a CI job claim.
type PodClaimer struct {
	client    kubernetes.Interface
	namespace string
}

// New creates a PodClaimer that validates tokens against the K8s API server.
// namespace is the Kubernetes namespace where ci-worker pods run.
func New(client kubernetes.Interface, namespace string) *PodClaimer {
	return &PodClaimer{client: client, namespace: namespace}
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

	// 2. Extract pod name from extra claims.
	podNames := result.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	if len(podNames) == 0 {
		return "", "", fmt.Errorf("pod name not present in token extra claims")
	}
	podName = string(podNames[0])

	// 3. Fetch the pod and check freshness.
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("fetch pod %q: %w", podName, err)
	}
	podAge := time.Since(pod.CreationTimestamp.Time)
	if podAge > maxPodAge {
		return "", "", fmt.Errorf("pod %q is too old (%s > %s)", podName, podAge.Round(time.Second), maxPodAge)
	}
	podIP = pod.Status.PodIP

	// 4. Pod must be owned by a batch/v1 Job.
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

	// 5. That Job must be owned by the ci-worker KEDA ScaledJob.
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
