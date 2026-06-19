// Package svcroute programs host ClusterIP Service routing for MacVz nodes.
//
// A MacVz node is a macOS host and cannot run kube-proxy (the kube-proxy Pod
// spec — Always restart, hostNetwork, privileged securityContext — is rejected
// by the provider), so nothing would otherwise translate a Service ClusterIP for
// a micro-VM. This controller watches Services and their EndpointSlices and
// programs the podnet pf anchor with `rdr` DNAT rules (ClusterIP:port -> ready
// backend), giving micro-VMs working ClusterIP Service access (#37/P5).
package svcroute

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chimerakang/macvz/pkg/network/podnet"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// Router is the subset of *podnet.Router the controller drives. An interface
// keeps the controller testable with a fake.
type Router interface {
	AttachService(ctx context.Context, key string, rules []podnet.ServiceRule) error
	DetachService(ctx context.Context, key string) error
}

// serviceGetter returns a Service by "namespace/name" key, or (nil, false) when
// it is absent. Backed by the Service informer cache in production.
type serviceGetter interface {
	get(key string) (*corev1.Service, bool)
}

// sliceLister returns the EndpointSlices owned by a Service. Backed by the
// EndpointSlice informer cache in production.
type sliceLister interface {
	listForService(namespace, serviceName string) ([]*discoveryv1.EndpointSlice, error)
}

// serviceKey returns the "namespace/name" key for a Service.
func serviceKey(ns, name string) string { return ns + "/" + name }

// BuildServiceRules computes the DNAT rules for a single ClusterIP Service from
// its ready EndpointSlices. It returns nil for Services that should not be
// programmed (headless, ExternalName, no ClusterIP, or no ready backends), so a
// nil result means "remove this Service's rules". It is pure, for easy testing.
func BuildServiceRules(svc *corev1.Service, slices []*discoveryv1.EndpointSlice) []podnet.ServiceRule {
	if svc == nil {
		return nil
	}
	// Only ClusterIP Services with a real VIP are programmable. Headless
	// ("None"), ExternalName, and not-yet-allocated Services are skipped.
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return nil
	}
	clusterIP := svc.Spec.ClusterIP
	if clusterIP == "" || clusterIP == corev1.ClusterIPNone {
		return nil
	}

	key := serviceKey(svc.Namespace, svc.Name)

	readyIPs := readyEndpointIPs(slices)
	if len(readyIPs) == 0 {
		return nil
	}

	// Map EndpointSlice port name -> numeric target port. A Service port matches
	// its slice port by name (the empty name covers a single unnamed port). All
	// slices of a Service carry the same port set, so we take the first value
	// seen for each name (first-write-wins over the ordered slice list) to stay
	// deterministic even if a transient slice disagrees.
	targetByName := map[string]int32{}
	for _, sl := range slices {
		for _, p := range sl.Ports {
			if p.Port == nil {
				continue
			}
			name := ""
			if p.Name != nil {
				name = *p.Name
			}
			if _, seen := targetByName[name]; !seen {
				targetByName[name] = *p.Port
			}
		}
	}

	rules := make([]podnet.ServiceRule, 0, len(svc.Spec.Ports))
	for _, sp := range svc.Spec.Ports {
		proto := strings.ToLower(string(sp.Protocol))
		if proto != "tcp" && proto != "udp" {
			continue // pf rdr here covers tcp/udp; SCTP is not supported
		}
		target, ok := targetByName[sp.Name]
		if !ok {
			// No EndpointSlice port advertised this service port's name yet.
			continue
		}
		rules = append(rules, podnet.ServiceRule{
			ServiceKey: key,
			ClusterIP:  clusterIP,
			Protocol:   proto,
			Port:       int(sp.Port),
			TargetPort: int(target),
			Backends:   readyIPs,
		})
	}
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// readyEndpointIPs collects the ready IPv4 endpoint addresses across all slices,
// deduplicated and sorted for deterministic output.
func readyEndpointIPs(slices []*discoveryv1.EndpointSlice) []string {
	seen := map[string]struct{}{}
	for _, sl := range slices {
		if sl.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		for _, ep := range sl.Endpoints {
			// A nil Ready means "ready" (slice spec, for backwards
			// compatibility); a terminating endpoint is never a target.
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			for _, addr := range ep.Addresses {
				seen[addr] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// Controller reconciles Services -> podnet rdr rules. It watches Services and
// EndpointSlices and, on any change, recomputes a Service's rules and pushes
// them to the Router.
type Controller struct {
	router    Router
	services  serviceGetter
	slices    sliceLister
	queue     workqueue.TypedRateLimitingInterface[string]
	hasSynced []cache.InformerSynced
}

// New builds a Controller from a cluster-wide (all-namespace) informer factory.
func New(router Router, factory informers.SharedInformerFactory) *Controller {
	svcInformer := factory.Core().V1().Services()
	sliceInformer := factory.Discovery().V1().EndpointSlices()

	c := newController(router,
		&informerServiceGetter{lister: svcInformer.Lister()},
		&informerSliceLister{lister: sliceInformer.Lister()},
		svcInformer.Informer().HasSynced,
		sliceInformer.Informer().HasSynced,
	)

	svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueService(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueService(obj) },
		DeleteFunc: func(obj any) { c.enqueueService(obj) },
	})
	sliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueSlice(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueSlice(obj) },
		DeleteFunc: func(obj any) { c.enqueueSlice(obj) },
	})
	return c
}

// newController is the dependency-injected constructor used by both New and the
// unit tests (which pass fakes and no informers).
func newController(router Router, svc serviceGetter, sl sliceLister, synced ...cache.InformerSynced) *Controller {
	return &Controller{
		router:   router,
		services: svc,
		slices:   sl,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		hasSynced: synced,
	}
}

func (c *Controller) enqueueService(obj any) {
	if key, ok := serviceKeyOf(obj); ok {
		c.queue.Add(key)
	}
}

// enqueueSlice maps an EndpointSlice to its owning Service via the well-known
// label and enqueues that Service.
func (c *Controller) enqueueSlice(obj any) {
	sl, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		tomb, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		sl, ok = tomb.Obj.(*discoveryv1.EndpointSlice)
		if !ok {
			return
		}
	}
	svc := sl.Labels[discoveryv1.LabelServiceName]
	if svc == "" {
		return
	}
	c.queue.Add(serviceKey(sl.Namespace, svc))
}

func serviceKeyOf(obj any) (string, bool) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return "", false
	}
	return serviceKey(svc.Namespace, svc.Name), true
}

// Run starts the controller: it waits for caches to sync, then processes the
// work queue until ctx is cancelled.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	klog.InfoS("starting service route controller")
	if !cache.WaitForCacheSync(ctx.Done(), c.hasSynced...) {
		return fmt.Errorf("svcroute: caches did not sync")
	}
	for i := 0; i < workers; i++ {
		go c.worker(ctx)
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) worker(ctx context.Context) {
	for c.processNext(ctx) {
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		klog.ErrorS(err, "reconcile service route failed; requeueing", "service", key)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// reconcile recomputes one Service's rules and pushes them to the Router. A
// deleted/headless/backend-less Service detaches its rules.
func (c *Controller) reconcile(ctx context.Context, key string) error {
	svc, ok := c.services.get(key)
	if !ok || svc == nil {
		return c.router.DetachService(ctx, key)
	}
	ns, name := splitKey(key)
	slices, err := c.slices.listForService(ns, name)
	if err != nil {
		return fmt.Errorf("list endpointslices for %q: %w", key, err)
	}
	rules := BuildServiceRules(svc, slices)
	if len(rules) == 0 {
		return c.router.DetachService(ctx, key)
	}
	return c.router.AttachService(ctx, key, rules)
}

func splitKey(key string) (ns, name string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func serviceSliceSelector(serviceName string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: serviceName})
}
