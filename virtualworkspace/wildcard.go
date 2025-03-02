/*
Copyright 2025 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package virtualworkspace

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	kcpinformers "github.com/kcp-dev/apimachinery/v2/third_party/informers"
	"github.com/kcp-dev/logicalcluster/v3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// WildcardCache is a cache that operates on a /clusters/* endpoint.
type WildcardCache interface {
	cache.Cache
	getSharedInformer(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error)
}

// NewWildcardCache returns a cache.Cache that handles multi-cluster watches
// against a /clusters/* endpoint. It wires SharedIndexInformers with additional
// indexes for cluster and cluster+namespace.
func NewWildcardCache(config *rest.Config, opts cache.Options) (WildcardCache, error) {
	config = rest.CopyConfig(config)
	config.Host = strings.TrimSuffix(config.Host, "/") + "/clusters/*"

	// setup everything we need to get a working REST mapper.
	if opts.Scheme == nil {
		opts.Scheme = scheme.Scheme
	}
	if opts.HTTPClient == nil {
		var err error
		opts.HTTPClient, err = rest.HTTPClientFor(config)
		if err != nil {
			return nil, fmt.Errorf("could not create HTTP client from config: %w", err)
		}
	}
	if opts.Mapper == nil {
		var err error
		opts.Mapper, err = apiutil.NewDynamicRESTMapper(config, opts.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("could not create RESTMapper from config: %w", err)
		}
	}

	ret := &wildcardCache{
		scheme: opts.Scheme,
		mapper: opts.Mapper,
		tracker: informerTracker{
			Structured:   make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
			Unstructured: make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
			Metadata:     make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
		},
	}

	opts.NewInformer = func(watcher k8scache.ListerWatcher, obj runtime.Object, duration time.Duration, indexers k8scache.Indexers) k8scache.SharedIndexInformer {
		gvk, err := apiutil.GVKForObject(obj, opts.Scheme)
		if err != nil {
			panic(err)
		}

		inf := kcpinformers.NewSharedIndexInformer(watcher, obj, duration, indexers)
		if err := inf.AddIndexers(k8scache.Indexers{
			ClusterIndexName:             ClusterIndexFunc,
			ClusterAndNamespaceIndexName: ClusterAndNamespaceIndexFunc,
		}); err != nil {
			utilruntime.HandleError(fmt.Errorf("unable to add cluster name indexers: %w", err))
		}

		infs := ret.tracker.informersByType(obj)
		ret.tracker.lock.Lock()
		if _, ok := infs[gvk]; ok {
			panic(fmt.Sprintf("informer for %s already exists", gvk))
		}
		infs[gvk] = inf
		ret.tracker.lock.Unlock()

		return inf
	}

	var err error
	ret.Cache, err = cache.New(config, opts)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

type sharedInformerGetter func(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error)

type wildcardCache struct {
	cache.Cache
	scheme  *runtime.Scheme
	mapper  apimeta.RESTMapper
	tracker informerTracker
}

func (c *wildcardCache) getSharedInformer(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error) {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return nil, gvk, "", false, err
	}

	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, gvk, "", false, err
	}

	infs := c.tracker.informersByType(obj)
	c.tracker.lock.RLock()
	inf, ok := infs[gvk]
	c.tracker.lock.RUnlock()

	return inf, gvk, mapping.Scope.Name(), ok, nil
}

// IndexField adds an index for the given object kind.
func (c *wildcardCache) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	return c.Cache.IndexField(ctx, obj, "cluster/"+field, func(obj client.Object) []string {
		keys := extractValue(obj)
		withCluster := make([]string, len(keys)*2)
		for i, key := range keys {
			withCluster[i] = fmt.Sprintf("%s/%s", logicalcluster.From(obj), key)
			withCluster[i+len(keys)] = fmt.Sprintf("*/%s", key)
		}
		return withCluster
	})
}

type informerTracker struct {
	lock         sync.RWMutex
	Structured   map[schema.GroupVersionKind]k8scache.SharedIndexInformer
	Unstructured map[schema.GroupVersionKind]k8scache.SharedIndexInformer
	Metadata     map[schema.GroupVersionKind]k8scache.SharedIndexInformer
}

func (t *informerTracker) informersByType(obj runtime.Object) map[schema.GroupVersionKind]k8scache.SharedIndexInformer {
	switch obj.(type) {
	case runtime.Unstructured:
		return t.Unstructured
	case *metav1.PartialObjectMetadata, *metav1.PartialObjectMetadataList:
		return t.Metadata
	default:
		return t.Structured
	}
}
