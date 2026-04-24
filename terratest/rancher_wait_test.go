package test

import "testing"

func TestSummarizeRancherPodsReady(t *testing.T) {
	ready, summary := summarizeRancherPods([]podView{
		{Name: "rancher-7f9d8b6b6b-abcde", Ready: "1/1", Status: "Running"},
		{Name: "rancher-webhook-6b885b7b47-fghij", Ready: "1/1", Status: "Running"},
	})

	if !ready {
		t.Fatalf("expected pods to be ready, got summary %q", summary)
	}
}

func TestSummarizeRancherPodsWaitsForWebhook(t *testing.T) {
	ready, summary := summarizeRancherPods([]podView{
		{Name: "rancher-7f9d8b6b6b-abcde", Ready: "1/1", Status: "Running"},
	})

	if ready {
		t.Fatal("expected pods not to be ready without rancher-webhook")
	}
	if summary == "" {
		t.Fatal("expected a useful summary")
	}
}

func TestSummarizeRancherPodsReportsUnreadyPods(t *testing.T) {
	ready, summary := summarizeRancherPods([]podView{
		{Name: "rancher-7f9d8b6b6b-abcde", Ready: "0/1", Status: "CrashLoopBackOff", Restarts: 3},
		{Name: "rancher-webhook-6b885b7b47-fghij", Ready: "1/1", Status: "Running"},
	})

	if ready {
		t.Fatal("expected pods not to be ready")
	}
	if summary == "" {
		t.Fatal("expected unready pod summary")
	}
}

func TestPodReadyForSignoff(t *testing.T) {
	cases := []struct {
		name string
		pod  podView
		want bool
	}{
		{name: "ready", pod: podView{Ready: "2/2", Status: "Running"}, want: true},
		{name: "partial", pod: podView{Ready: "1/2", Status: "Running"}, want: false},
		{name: "pending", pod: podView{Ready: "0/1", Status: "Pending"}, want: false},
		{name: "bad ready", pod: podView{Ready: "soon", Status: "Running"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podReadyForSignoff(tc.pod); got != tc.want {
				t.Fatalf("podReadyForSignoff() = %v, want %v", got, tc.want)
			}
		})
	}
}
