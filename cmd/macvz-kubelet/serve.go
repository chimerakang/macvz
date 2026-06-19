package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/network/svcroute"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
	"github.com/chimerakang/macvz/pkg/provider"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// configMapLister adapts a ConfigMap informer's lister to the provider's
// ConfigMapGetter so Pods can consume ConfigMap-backed env vars and volumes
// (#46) from the controller's already-synced cache.
type configMapLister struct {
	lister corev1listers.ConfigMapLister
}

func (c configMapLister) GetConfigMap(namespace, name string) (*corev1.ConfigMap, error) {
	return c.lister.ConfigMaps(namespace).Get(name)
}

// clientsetTokenCreator mints bound service-account tokens through the live
// clientset's TokenRequest subresource, so Pods get normal in-cluster API access
// (#51). It is the namespace-aware adapter the provider's token issuer needs.
type clientsetTokenCreator struct {
	clientset kubernetes.Interface
}

func (c clientsetTokenCreator) CreateToken(ctx context.Context, namespace, serviceAccountName string, tr *authnv1.TokenRequest, opts metav1.CreateOptions) (*authnv1.TokenRequest, error) {
	return c.clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccountName, tr, opts)
}

// informerResync is how often the shared informers do a full relist.
const informerResync = time.Minute

// podCIDRWaitTimeout bounds how long startup waits for Kubernetes to assign this
// node a Pod CIDR before continuing without coordinated IPAM.
const podCIDRWaitTimeout = 30 * time.Second

const (
	apiReachabilityTimeout = 30 * time.Second
	apiReachabilityProbe   = 5 * time.Second
	apiReachabilityRetry   = time.Second
	kubeletListenTimeout   = 10 * time.Second
	kubeletListenRetry     = 250 * time.Millisecond
)

// waitForAPIServer verifies the Kubernetes API is reachable after the host data
// plane has finished mutating routes. This keeps route churn failures in the
// startup path, before Virtual Kubelet starts its long-lived controllers.
func waitForAPIServer(ctx context.Context, clientset kubernetes.Interface) error {
	return waitForAPIServerWithTimeout(ctx, clientset, apiReachabilityTimeout, apiReachabilityProbe, apiReachabilityRetry)
}

func waitForAPIServerWithTimeout(ctx context.Context, clientset kubernetes.Interface, totalTimeout, probeTimeout, retryDelay time.Duration) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	var lastErr error
	for {
		errCh := make(chan error, 1)
		go func() {
			_, err := clientset.Discovery().ServerVersion()
			errCh <- err
		}()

		select {
		case err := <-errCh:
			if err == nil {
				klog.InfoS("Kubernetes API reachable after data-plane setup")
				return nil
			}
			lastErr = err
			klog.ErrorS(err, "Kubernetes API reachability probe failed; retrying")
		case <-time.After(probeTimeout):
			lastErr = fmt.Errorf("API probe timed out after %s", probeTimeout)
			klog.ErrorS(lastErr, "Kubernetes API reachability probe failed; retrying")
		case <-deadlineCtx.Done():
			if lastErr == nil {
				lastErr = deadlineCtx.Err()
			}
			return fmt.Errorf("API did not answer within %s: %w", totalTimeout, lastErr)
		}

		select {
		case <-deadlineCtx.Done():
			if lastErr == nil {
				lastErr = deadlineCtx.Err()
			}
			return fmt.Errorf("API did not answer within %s: %w", totalTimeout, lastErr)
		case <-time.After(retryDelay):
		}
	}
}

// setupIPAM enables coordinated Pod IPAM for this node. The address range is the
// node's Kubernetes-assigned Spec.PodCIDR (or cfg.Node.PodCIDR when set as an
// override). It then recovers existing allocations from the API server so a
// restart neither leaks addresses nor reassigns a live Pod's IP.
//
// IPAM is best-effort: on a cluster that assigns no Pod CIDR (and with no
// override configured), it logs and returns nil so Pods still run with the Pod
// IP derived from the runtime-reported address.
func setupIPAM(ctx context.Context, cfg config.Config, clientset *kubernetes.Clientset, p *provider.Provider) error {
	cidr := cfg.Node.PodCIDR
	if cidr == "" {
		var err error
		cidr, err = waitForPodCIDR(ctx, clientset, cfg.NodeName)
		if err != nil {
			klog.ErrorS(err, "coordinated Pod IPAM disabled; Pod IPs will come from the runtime",
				"hint", "run kube-controller-manager with --allocate-node-cidrs or set node.podCIDR")
			return nil
		}
	}

	ipam, err := network.NewPodIPAM(cidr)
	if err != nil {
		return fmt.Errorf("build pod IPAM for %q: %w", cidr, err)
	}
	p.SetIPAM(ipam)
	klog.InfoS("coordinated Pod IPAM enabled", "node", cfg.NodeName, "podCIDR", ipam.CIDR())

	// Rebuild allocations from Kubernetes state before the Pod controller runs.
	podList, err := clientset.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", cfg.NodeName).String(),
	})
	if err != nil {
		// A failed recovery list is non-fatal: the allocator starts empty and
		// re-derives IPs as Pods are (re)created.
		klog.ErrorS(err, "could not list existing Pods for IPAM recovery", "node", cfg.NodeName)
		return nil
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}
	p.RecoverAllocations(pods)
	return nil
}

// waitForPodCIDR polls the node until Kubernetes assigns its Spec.PodCIDR, which
// happens shortly after registration on clusters with node-CIDR allocation.
func waitForPodCIDR(ctx context.Context, clientset *kubernetes.Clientset, nodeName string) (string, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, podCIDRWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		node, err := clientset.CoreV1().Nodes().Get(deadlineCtx, nodeName, metav1.GetOptions{})
		if err == nil && node.Spec.PodCIDR != "" {
			return node.Spec.PodCIDR, nil
		}
		select {
		case <-deadlineCtx.Done():
			return "", fmt.Errorf("node %q has no Spec.PodCIDR after %s", nodeName, podCIDRWaitTimeout)
		case <-ticker.C:
		}
	}
}

// startPodController wires the Virtual Kubelet pod controller: pod/configmap/
// secret/service informers (pods filtered to this node), an event recorder, and
// the controller itself driving the provider's Pod lifecycle. It returns a
// cleanup func to release the event broadcaster.
func startPodController(ctx context.Context, cfg config.Config, clientset *kubernetes.Clientset, p *provider.Provider, workers int) (func(), error) {
	podFactory := informers.NewSharedInformerFactoryWithOptions(clientset, informerResync, nodeutil.PodInformerFilter(cfg.NodeName))
	scmFactory := informers.NewSharedInformerFactoryWithOptions(clientset, informerResync)

	podInformer := podFactory.Core().V1().Pods()
	secretInformer := scmFactory.Core().V1().Secrets()
	configMapInformer := scmFactory.Core().V1().ConfigMaps()
	serviceInformer := scmFactory.Core().V1().Services()

	// Resolve ConfigMap-backed env vars and volumes from the controller's own
	// ConfigMap cache (#46). Wired in before the controller starts reconciling.
	p.SetConfigMapGetter(configMapLister{lister: configMapInformer.Lister()})

	// Resolve imagePullSecrets from the controller's own Secret cache (#49), so
	// private images are pulled with the Pod's credentials. Wired in before the
	// controller starts reconciling.
	p.SetSecretGetter(provider.NewSecretLister(secretInformer.Lister()))

	// Issue bound service-account tokens through the clientset's TokenRequest API
	// so Pods get the projected kube-api-access volume and normal in-cluster API
	// access (#51). Wired in before the controller starts reconciling.
	p.SetTokenRequester(provider.NewTokenRequester(clientsetTokenCreator{clientset: clientset}))

	eb := record.NewBroadcaster()
	eb.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: clientset.CoreV1().Events(corev1.NamespaceAll)})
	recorder := eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: path.Join(cfg.NodeName, "pod-controller")})

	pc, err := vknode.NewPodController(vknode.PodControllerConfig{
		PodClient:         clientset.CoreV1(),
		EventRecorder:     recorder,
		Provider:          p,
		PodInformer:       podInformer,
		SecretInformer:    secretInformer,
		ConfigMapInformer: configMapInformer,
		ServiceInformer:   serviceInformer,
	})
	if err != nil {
		eb.Shutdown()
		return nil, fmt.Errorf("create pod controller: %w", err)
	}

	podFactory.Start(ctx.Done())
	scmFactory.Start(ctx.Done())

	go func() {
		if err := pc.Run(ctx, workers); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "pod controller stopped unexpectedly")
		}
	}()

	select {
	case <-pc.Ready():
		klog.InfoS("pod controller ready", "node", cfg.NodeName, "workers", workers)
	case <-pc.Done():
		eb.Shutdown()
		return nil, fmt.Errorf("pod controller exited before becoming ready: %w", pc.Err())
	case <-ctx.Done():
	}

	return eb.Shutdown, nil
}

// setupMesh brings up this node's WireGuard mesh when enabled, returning a
// cleanup func that tears it back down. The mesh encrypts and routes cross-host
// Pod traffic (issue #21); each peer's Pod CIDR becomes a route through the
// tunnel. When the mesh is disabled it is a no-op returning a no-op cleanup.
//
// When configPath is set, it also starts a SIGHUP-driven reconciler (#42): an
// operator edits the config's peer list and signals the kubelet to add/remove
// peers and routes in place, with no restart and no disruption to local Pod
// attachments.
func setupMesh(ctx context.Context, cfg config.Config, configPath string) (func(), error) {
	if !cfg.Mesh.Enabled {
		klog.InfoS("WireGuard mesh disabled; Pods are reachable only on their local node")
		return func() {}, nil
	}

	ifc, err := cfg.MeshInterfaceConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve mesh config: %w", err)
	}
	var meshOpts []wireguard.Option
	if cfg.PrivilegedHelperSocket != "" {
		meshOpts = append(meshOpts, wireguard.WithHelperSocket(cfg.PrivilegedHelperSocket))
	}
	mesh := wireguard.New(ifc, meshOpts...)
	if err := mesh.Up(ctx); err != nil {
		return nil, fmt.Errorf("bring up mesh interface %q: %w", ifc.Name, err)
	}
	klog.InfoS("WireGuard mesh up",
		"interface", mesh.InterfaceName(),
		"publicKey", ifc.PrivateKey.PublicKey().String(),
		"peers", mesh.Peers(),
		"routes", mesh.InstalledRoutes(),
	)

	// Reconcile peers on SIGHUP so nodes can join/leave without a restart (#42).
	// Requires the config path to reload; with no --config it is skipped.
	if configPath != "" {
		go watchMeshReload(ctx, mesh, configPath, cfg.PrivilegedHelperSocket)
		klog.InfoS("mesh peer reload enabled; edit peers then `kill -HUP` the kubelet to reconcile", "config", configPath)
	}

	return func() {
		// Tear down with a fresh context: the root ctx is already cancelled by
		// the time cleanup runs during shutdown.
		if err := mesh.Down(context.Background()); err != nil {
			klog.ErrorS(err, "failed to tear down mesh", "interface", ifc.Name)
		}
	}, nil
}

// setupPodNetwork starts the host Pod network path (when enabled) and attaches
// it to the provider so each micro-VM is reachable at its Pod IP across the mesh
// (#22). It returns a cleanup func that flushes the host rules. When disabled it
// is a no-op returning a no-op cleanup.
func setupPodNetwork(ctx context.Context, cfg config.Config, p *provider.Provider) (*podnet.Router, func(), error) {
	if !cfg.PodNetwork.Enabled {
		klog.InfoS("Pod network path disabled; Pods keep the runtime host-only address")
		return nil, func() {}, nil
	}

	var pnOpts []podnet.Option
	if cfg.PrivilegedHelperSocket != "" {
		pnOpts = append(pnOpts, podnet.WithHelperSocket(cfg.PrivilegedHelperSocket))
	}
	router := podnet.New(cfg.PodNetworkRouterConfig(), pnOpts...)
	if err := router.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start pod network path: %w", err)
	}
	p.SetPodNetwork(router)
	klog.InfoS("Pod network path started", "interface", cfg.PodNetwork.Interface)

	return router, func() {
		if err := router.Stop(context.Background()); err != nil {
			klog.ErrorS(err, "failed to stop pod network path")
		}
	}, nil
}

// startServiceController runs the ClusterIP Service route controller (#37). It
// watches Services and EndpointSlices cluster-wide and programs the podnet
// anchor with rdr DNAT rules so micro-VMs can reach Service ClusterIPs. It is a
// no-op (nil router) when the Pod network path is disabled — without it there is
// nothing to program. Returns a cleanup that stops the controller.
func startServiceController(ctx context.Context, cfg config.Config, clientset *kubernetes.Clientset, router *podnet.Router) func() {
	if router == nil {
		klog.InfoS("ClusterIP service routing disabled (Pod network path is off)")
		return func() {}
	}
	factory := informers.NewSharedInformerFactory(clientset, informerResync)
	ctrl := svcroute.New(router, factory)

	ctlCtx, cancel := context.WithCancel(ctx)
	factory.Start(ctlCtx.Done())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := ctrl.Run(ctlCtx, serviceControllerWorkers); err != nil && ctlCtx.Err() == nil {
			klog.ErrorS(err, "service route controller stopped unexpectedly")
		}
	}()
	klog.InfoS("ClusterIP service routing enabled", "interface", cfg.PodNetwork.Interface)
	// Wait for the controller to drain on cleanup so no reconcile races the
	// router's Stop (the Pod network path is torn down right after this).
	return func() {
		cancel()
		<-done
	}
}

// serviceControllerWorkers is the concurrency of the service route controller.
const serviceControllerWorkers = 2

// buildServingTLSConfig assembles the TLS config for the kubelet HTTPS server.
// When clientCAFile is set, the server requires and verifies a client
// certificate signed by that CA (mutual TLS), so only holders of an
// API-server-issued client cert can reach logs/exec/portforward/stats.
// Otherwise it accepts any TLS client and relies on network restriction.
func buildServingTLSConfig(cert tls.Certificate, clientCAFile string) (*tls.Config, error) {
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read kubelet client CA %q: %w", clientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("kubelet client CA %q contains no usable certificates", clientCAFile)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// startKubeletServer starts the HTTPS kubelet API used by `kubectl logs`/`exec`,
// routing to the provider. It is a no-op (returning a no-op cleanup) when no
// serving certificate is configured, mirroring upstream Virtual Kubelet: Pods
// still run, but logs/exec are unavailable until certs are provided.
//
// The endpoint exposes logs/exec/portforward/stats, so it is hardened (#28):
// when a client CA is configured it requires mutual TLS (only the API server
// can reach it); it binds to the node's reachable address rather than all
// interfaces when listenIP is known; and it warns loudly when left
// unauthenticated so that exposure is a deliberate, network-restricted choice.
func startKubeletServer(ctx context.Context, cfg config.Config, p *provider.Provider, listenIP string) (func(), error) {
	if cfg.Node.ServingTLSCertFile == "" || cfg.Node.ServingTLSKeyFile == "" {
		klog.InfoS("kubelet TLS serving disabled (no servingTLSCertFile/KeyFile); kubectl logs/exec will be unavailable")
		return func() {}, nil
	}

	cert, err := tls.LoadX509KeyPair(cfg.Node.ServingTLSCertFile, cfg.Node.ServingTLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load serving TLS keypair: %w", err)
	}

	tlsCfg, err := buildServingTLSConfig(cert, cfg.Node.ServingClientCAFile)
	if err != nil {
		return nil, err
	}
	if cfg.Node.ServingClientCAFile == "" {
		klog.InfoS("WARNING: kubelet endpoint has no client authentication (node.servingClientCAFile unset); " +
			"logs/exec/portforward/stats are reachable by anyone who can connect. Restrict it by network (bind address + firewall) " +
			"or set servingClientCAFile to require API-server mutual TLS.")
	}

	handler := vkapi.PodHandler(vkapi.PodHandlerConfig{
		RunInContainer:        p.RunInContainer,
		GetContainerLogs:      p.GetContainerLogs,
		PortForward:           p.PortForward,
		GetPods:               p.GetPods,
		GetPodsFromKubernetes: p.GetPods,
		GetStatsSummary:       p.StatsSummary,
		GetMetricsResource:    p.MetricsResource,
	}, false)

	// Bind to the node's reachable address when known, rather than all
	// interfaces, to minimize the endpoint's exposure.
	port := fmt.Sprintf("%d", cfg.Node.KubeletPort)
	addr := net.JoinHostPort("", port)
	if listenIP != "" {
		addr = net.JoinHostPort(listenIP, port)
	} else {
		klog.InfoS("kubelet endpoint binding to all interfaces (no internal IP resolved); consider setting node.internalIP", "port", port)
	}
	listener, err := listenKubeletTLS(ctx, addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed && ctx.Err() == nil {
			klog.ErrorS(err, "kubelet API server stopped unexpectedly")
		}
	}()
	klog.InfoS("kubelet API server listening", "addr", addr, "clientAuth", cfg.Node.ServingClientCAFile != "")

	return func() {
		_ = srv.Close()
		_ = listener.Close()
	}, nil
}

func listenKubeletTLS(ctx context.Context, addr string, tlsCfg *tls.Config) (net.Listener, error) {
	return listenKubeletTLSWithRetry(ctx, addr, tlsCfg, kubeletListenTimeout, kubeletListenRetry)
}

func listenKubeletTLSWithRetry(ctx context.Context, addr string, tlsCfg *tls.Config, totalTimeout, retryDelay time.Duration) (net.Listener, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	var lastErr error
	for {
		ln, err := tls.Listen("tcp", addr, tlsCfg)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
		lastErr = err
		klog.ErrorS(err, "kubelet API port still in use; retrying", "addr", addr)

		select {
		case <-deadlineCtx.Done():
			return nil, fmt.Errorf("address still in use after %s: %w", totalTimeout, lastErr)
		case <-time.After(retryDelay):
		}
	}
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
