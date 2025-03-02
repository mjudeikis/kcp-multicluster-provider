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

	"github.com/kcp-dev/logicalcluster/v3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
	c := rest.CopyConfig(config)
	c.Host = strings.TrimSuffix(c.Host, "/") + "/clusters/*"

	opts, sharedInformers := withClusterIndexes(opts)
	ca, err := cache.New(c, opts)

	return &wildcardCache{Cache: ca, sharedInformerGetter: sharedInformers}, err
}

type sharedInformerGetter func(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error)

type wildcardCache struct {
	cache.Cache
	sharedInformerGetter
}

func (c *wildcardCache) getSharedInformer(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error) {
	return c.sharedInformerGetter(obj)
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

// withClusterIndexes wires an informer constructor that adds ClusterIndexName
// and ClusterAndNamespaceIndexName indexes and gives access to the underlying
// SharedIndexInformers. We need that to access the indexes directly.
func withClusterIndexes(opts cache.Options) (cache.Options, sharedInformerGetter) {
	tracker := informerTracker{
		Structured:   make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
		Unstructured: make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
		Metadata:     make(map[schema.GroupVersionKind]k8scache.SharedIndexInformer),
	}

	opts.NewInformer = func(watcher k8scache.ListerWatcher, obj runtime.Object, duration time.Duration, indexers k8scache.Indexers) k8scache.SharedIndexInformer {
		gvk, err := apiutil.GVKForObject(obj, opts.Scheme)
		if err != nil {
			panic(err)
		}

		inf := k8scache.NewSharedIndexInformer(watcher, obj, duration, indexers)
		if err := inf.AddIndexers(k8scache.Indexers{
			ClusterIndexName:             ClusterIndexFunc,
			ClusterAndNamespaceIndexName: ClusterAndNamespaceIndexFunc,
		}); err != nil {
			utilruntime.HandleError(fmt.Errorf("unable to add cluster name indexers: %w", err))
		}

		infs := tracker.informersByType(obj)
		tracker.lock.Lock()
		if _, ok := infs[gvk]; ok {
			panic(fmt.Sprintf("informer for %s already exists", gvk))
		}
		infs[gvk] = inf
		tracker.lock.Unlock()

		return inf
	}

	return opts, func(obj runtime.Object) (k8scache.SharedIndexInformer, schema.GroupVersionKind, apimeta.RESTScopeName, bool, error) {
		gvk, err := apiutil.GVKForObject(obj, opts.Scheme)
		if err != nil {
			return nil, gvk, "", false, err
		}

		mapping, err := opts.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, gvk, "", false, err
		}

		infs := tracker.informersByType(obj)
		tracker.lock.RLock()
		inf, ok := infs[gvk]
		tracker.lock.RUnlock()

		return inf, gvk, mapping.Scope.Name(), ok, nil
	}
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
