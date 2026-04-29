package k8sproof_test

import (
	"context"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlorenc/docstore/internal/k8sproof"
)

const (
	ns      = "docstore-ci"
	saToken = "test-sa-token"
)

// tokenReviewReactor returns a fake reactor for TokenReview creates.
func tokenReviewReactor(authenticated bool, username, podName string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		result := &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: authenticated,
				User: authv1.UserInfo{
					Username: username,
					Extra: map[string]authv1.ExtraValue{
						"authentication.kubernetes.io/pod-name": {podName},
					},
				},
			},
		}
		if !authenticated {
			result.Status.Error = "token invalid"
		}
		return true, result, nil
	}
}

// validTokenReviewReactor returns a reactor for a valid ci-worker token for the given podName.
func validTokenReviewReactor(podName string) k8stesting.ReactionFunc {
	return tokenReviewReactor(true, "system:serviceaccount:docstore-ci:ci-worker", podName)
}

// freshPod returns a pod with a fresh creationTimestamp owned by the given job.
func freshPod(podName, jobName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       jobName,
				},
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}
}

// jobOwnedByScaledJob returns a batch/v1 Job owned by the named ScaledJob.
func jobOwnedByScaledJob(jobName, scaledJobName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "keda.sh/v1alpha1",
					Kind:       "ScaledJob",
					Name:       scaledJobName,
				},
			},
		},
	}
}

func TestValidateToken_HappyPath(t *testing.T) {
	podName := "ci-worker-abc"
	jobName := "ci-worker-job-1"

	client := k8sfake.NewClientset(
		freshPod(podName, jobName),
		jobOwnedByScaledJob(jobName, "ci-worker"),
	)
	client.PrependReactor("create", "tokenreviews", validTokenReviewReactor(podName))

	claimer := k8sproof.New(client, ns)
	gotPod, gotIP, err := claimer.ValidateToken(context.Background(), saToken)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if gotPod != podName {
		t.Errorf("pod name: got %q, want %q", gotPod, podName)
	}
	if gotIP != "10.0.0.1" {
		t.Errorf("pod IP: got %q, want %q", gotIP, "10.0.0.1")
	}
}

func TestValidateToken_BadToken(t *testing.T) {
	client := k8sfake.NewClientset()
	client.PrependReactor("create", "tokenreviews",
		tokenReviewReactor(false, "", ""))

	claimer := k8sproof.New(client, ns)
	_, _, err := claimer.ValidateToken(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestValidateToken_WrongSA(t *testing.T) {
	podName := "some-pod"
	client := k8sfake.NewClientset()
	client.PrependReactor("create", "tokenreviews",
		tokenReviewReactor(true, "system:serviceaccount:other-ns:other-sa", podName))

	claimer := k8sproof.New(client, ns)
	_, _, err := claimer.ValidateToken(context.Background(), saToken)
	if err == nil {
		t.Fatal("expected error for wrong SA, got nil")
	}
}

func TestValidateToken_StalePod(t *testing.T) {
	podName := "ci-worker-stale"
	jobName := "ci-worker-job-2"

	stalePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-20 * time.Minute)),
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "batch/v1", Kind: "Job", Name: jobName},
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.2"},
	}

	client := k8sfake.NewClientset(stalePod, jobOwnedByScaledJob(jobName, "ci-worker"))
	client.PrependReactor("create", "tokenreviews", validTokenReviewReactor(podName))

	claimer := k8sproof.New(client, ns)
	_, _, err := claimer.ValidateToken(context.Background(), saToken)
	if err == nil {
		t.Fatal("expected error for stale pod, got nil")
	}
}

func TestValidateToken_MissingJobOwner(t *testing.T) {
	podName := "ci-worker-nojob"
	// Pod has no ownerReferences — should fail at check 4.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.3"},
	}

	client := k8sfake.NewClientset(pod)
	client.PrependReactor("create", "tokenreviews", validTokenReviewReactor(podName))

	claimer := k8sproof.New(client, ns)
	_, _, err := claimer.ValidateToken(context.Background(), saToken)
	if err == nil {
		t.Fatal("expected error for missing job owner, got nil")
	}
}

func TestValidateToken_WrongScaledJobName(t *testing.T) {
	podName := "ci-worker-wrongsj"
	jobName := "ci-worker-job-3"

	client := k8sfake.NewClientset(
		freshPod(podName, jobName),
		jobOwnedByScaledJob(jobName, "some-other-scaled-job"),
	)
	client.PrependReactor("create", "tokenreviews", validTokenReviewReactor(podName))

	claimer := k8sproof.New(client, ns)
	_, _, err := claimer.ValidateToken(context.Background(), saToken)
	if err == nil {
		t.Fatal("expected error for wrong ScaledJob name, got nil")
	}
}
