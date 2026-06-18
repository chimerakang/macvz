package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/provider"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// informerResync is how often the shared informers do a full relist.
const informerResync = time.Minute

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

// startKubeletServer starts the HTTPS kubelet API used by `kubectl logs`/`exec`,
// routing to the provider. It is a no-op (returning a no-op cleanup) when no
// serving certificate is configured, mirroring upstream Virtual Kubelet: Pods
// still run, but logs/exec are unavailable until certs are provided.
func startKubeletServer(ctx context.Context, cfg config.Config, p *provider.Provider) (func(), error) {
	if cfg.Node.ServingTLSCertFile == "" || cfg.Node.ServingTLSKeyFile == "" {
		klog.InfoS("kubelet TLS serving disabled (no servingTLSCertFile/KeyFile); kubectl logs/exec will be unavailable")
		return func() {}, nil
	}

	cert, err := tls.LoadX509KeyPair(cfg.Node.ServingTLSCertFile, cfg.Node.ServingTLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load serving TLS keypair: %w", err)
	}

	handler := vkapi.PodHandler(vkapi.PodHandlerConfig{
		RunInContainer:        p.RunInContainer,
		GetContainerLogs:      p.GetContainerLogs,
		GetPods:               p.GetPods,
		GetPodsFromKubernetes: p.GetPods,
	}, false)

	addr := fmt.Sprintf(":%d", cfg.Node.KubeletPort)
	listener, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed && ctx.Err() == nil {
			klog.ErrorS(err, "kubelet API server stopped unexpectedly")
		}
	}()
	klog.InfoS("kubelet API server listening", "addr", addr)

	return func() {
		_ = srv.Close()
		_ = listener.Close()
	}, nil
}
