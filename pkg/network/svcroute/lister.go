package svcroute

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
)

// informerServiceGetter adapts the Service informer lister to serviceGetter.
type informerServiceGetter struct {
	lister corev1listers.ServiceLister
}

func (g *informerServiceGetter) get(key string) (*corev1.Service, bool) {
	ns, name := splitKey(key)
	svc, err := g.lister.Services(ns).Get(name)
	if err != nil || svc == nil {
		return nil, false
	}
	return svc, true
}

// informerSliceLister adapts the EndpointSlice informer lister to sliceLister,
// selecting a Service's slices by the well-known service-name label.
type informerSliceLister struct {
	lister discoverylisters.EndpointSliceLister
}

func (l *informerSliceLister) listForService(namespace, serviceName string) ([]*discoveryv1.EndpointSlice, error) {
	return l.lister.EndpointSlices(namespace).List(serviceSliceSelector(serviceName))
}
