package provider

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func dnsPod(ns string, policy corev1.DNSPolicy, dc *corev1.PodDNSConfig) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"},
		Spec:       corev1.PodSpec{DNSPolicy: policy, DNSConfig: dc},
	}
}

func TestResolveDNSClusterFirst(t *testing.T) {
	cfg := DNSConfig{ClusterDNS: []string{"10.96.0.10"}, ClusterDomain: "cluster.local"}
	ns, search, opts := resolveDNS(dnsPod("team-a", corev1.DNSClusterFirst, nil), cfg)

	if !reflect.DeepEqual(ns, []string{"10.96.0.10"}) {
		t.Errorf("nameservers = %v", ns)
	}
	wantSearch := []string{"team-a.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	if !reflect.DeepEqual(search, wantSearch) {
		t.Errorf("search = %v, want %v", search, wantSearch)
	}
	if !reflect.DeepEqual(opts, []string{"ndots:5"}) {
		t.Errorf("options = %v", opts)
	}
}

func TestResolveDNSDefaultPolicyEmptyWhenUnset(t *testing.T) {
	// Empty DNSPolicy behaves as ClusterFirst; with no cluster DNS configured it
	// injects nothing (single-host behavior is unchanged).
	ns, search, opts := resolveDNS(dnsPod("default", "", nil), DNSConfig{})
	if ns != nil || search != nil || opts != nil {
		t.Errorf("expected no injection, got ns=%v search=%v opts=%v", ns, search, opts)
	}
}

func TestResolveDNSDomainDefault(t *testing.T) {
	cfg := DNSConfig{ClusterDNS: []string{"10.96.0.10"}} // no domain -> default
	_, search, _ := resolveDNS(dnsPod("default", corev1.DNSClusterFirst, nil), cfg)
	if search[len(search)-1] != "cluster.local" {
		t.Errorf("default domain not applied: %v", search)
	}
}

func TestResolveDNSPolicyDefaultInjectsNothing(t *testing.T) {
	cfg := DNSConfig{ClusterDNS: []string{"10.96.0.10"}}
	ns, search, opts := resolveDNS(dnsPod("default", corev1.DNSDefault, nil), cfg)
	if ns != nil || search != nil || opts != nil {
		t.Errorf("DNSDefault should inject nothing, got ns=%v search=%v opts=%v", ns, search, opts)
	}
}

func TestResolveDNSNoneUsesPodConfig(t *testing.T) {
	val := "2"
	dc := &corev1.PodDNSConfig{
		Nameservers: []string{"1.1.1.1"},
		Searches:    []string{"example.com"},
		Options:     []corev1.PodDNSConfigOption{{Name: "ndots", Value: &val}, {Name: "edns0"}},
	}
	ns, search, opts := resolveDNS(dnsPod("default", corev1.DNSNone, dc), DNSConfig{ClusterDNS: []string{"10.96.0.10"}})
	if !reflect.DeepEqual(ns, []string{"1.1.1.1"}) {
		t.Errorf("None policy should use pod nameservers, got %v", ns)
	}
	if !reflect.DeepEqual(search, []string{"example.com"}) {
		t.Errorf("search = %v", search)
	}
	if !reflect.DeepEqual(opts, []string{"ndots:2", "edns0"}) {
		t.Errorf("options = %v", opts)
	}
}

func TestResolveDNSClusterFirstMergesPodExtras(t *testing.T) {
	cfg := DNSConfig{ClusterDNS: []string{"10.96.0.10"}, ClusterDomain: "cluster.local"}
	dc := &corev1.PodDNSConfig{Nameservers: []string{"8.8.8.8"}, Searches: []string{"extra.local"}}
	ns, search, _ := resolveDNS(dnsPod("default", corev1.DNSClusterFirst, dc), cfg)
	if len(ns) != 2 || ns[1] != "8.8.8.8" {
		t.Errorf("expected merged nameservers, got %v", ns)
	}
	found := false
	for _, s := range search {
		if s == "extra.local" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected merged search domain, got %v", search)
	}
}
