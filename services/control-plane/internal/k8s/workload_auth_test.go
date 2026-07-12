package k8s

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestVerifyWorkloadTokenBinding(t *testing.T) {
	tests := []struct {
		name       string
		username   string
		audiences  []string
		claimedUID string
		wantErr    bool
	}{
		{
			name:       "valid projected identity",
			username:   "system:serviceaccount:netclode:github-bot",
			audiences:  []string{"netclode-client"},
			claimedUID: "pod-uid",
		},
		{
			name:       "wrong service account",
			username:   "system:serviceaccount:netclode:default",
			audiences:  []string{"netclode-client"},
			claimedUID: "pod-uid",
			wantErr:    true,
		},
		{
			name:       "wrong audience",
			username:   "system:serviceaccount:netclode:github-bot",
			audiences:  []string{"other"},
			claimedUID: "pod-uid",
			wantErr:    true,
		},
		{
			name:       "stale pod token",
			username:   "system:serviceaccount:netclode:github-bot",
			audiences:  []string{"netclode-client"},
			claimedUID: "old-pod-uid",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "github-bot-pod", Namespace: "netclode", UID: types.UID("pod-uid")},
			})
			client.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     tt.audiences,
					User: authenticationv1.UserInfo{
						Username: tt.username,
						Extra: map[string]authenticationv1.ExtraValue{
							"authentication.kubernetes.io/pod-name": {"github-bot-pod"},
							"authentication.kubernetes.io/pod-uid":  {tt.claimedUID},
						},
					},
				}}, nil
			})

			r := &k8sRuntime{clientset: client, namespace: "netclode"}
			_, err := r.VerifyWorkloadToken(context.Background(), "token", []string{"netclode-client"}, "github-bot")
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifyWorkloadToken() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
