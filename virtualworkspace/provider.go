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
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1alpha1 "github.com/kcp-dev/kcp/sdk/apis/core/v1alpha1"
	"github.com/kcp-dev/logicalcluster/v3"
	mcmanager "github.com/multicluster-runtime/multicluster-runtime/pkg/manager"
	"github.com/multicluster-runtime/multicluster-runtime/pkg/multicluster"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var _ multicluster.Provider = &Provider{}

// Provider is a cluster provider that represents each logical cluster in the
// kcp sense as a cluster in the multicluster-runtime sense.
type Provider struct {
	config *rest.Config
	scheme *runtime.Scheme
	cache  WildcardCache

	log logr.Logger

	lock      sync.RWMutex
	clusters  map[logicalcluster.Name]cluster.Cluster
	cancelFns map[logicalcluster.Name]context.CancelFunc
}

type Options struct {
	Scheme *runtime.Scheme
	Cache  WildcardCache
}

// New creates a new kcp virtual workspace provider. The provided rest.Config
// must point to a virtual workspace apiserver base path, i.e. up to but without
// the "/clusters/*" suffix.
func New(cfg *rest.Config, options Options) (*Provider, error) {
	if options.Scheme == nil {
		options.Scheme = scheme.Scheme
	}
	if options.Cache == nil {
		var err error
		options.Cache, err = NewWildcardCache(cfg, cache.Options{})
		if err != nil {
			return nil, fmt.Errorf("failed to create wildcard cache: %w", err)
		}
	}

	return &Provider{
		config: cfg,
		scheme: options.Scheme,
		cache:  options.Cache,

		log: log.Log.WithName("kcp-virtualworkspace-cluster-provider"),

		clusters:  map[logicalcluster.Name]cluster.Cluster{},
		cancelFns: map[logicalcluster.Name]context.CancelFunc{},
	}, nil
}

// Run starts the provider and blocks.
func (p *Provider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	g, ctx := errgroup.WithContext(ctx)

	// Watch logical clusters and engage them as clusters in multicluster-runtime.
	lc := &corev1alpha1.LogicalCluster{}
	lcInf, err := p.cache.GetInformer(ctx, lc, cache.BlockUntilSynced(false))
	if err != nil {
		return fmt.Errorf("failed to get logical cluster informer: %w", err)
	}
	lcInf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			lc, ok := obj.(*corev1alpha1.LogicalCluster)
			if !ok {
				klog.Errorf("unexpected object type %T", obj)
				return
			}
			clusterName := logicalcluster.From(lc)

			// fast path.
			p.lock.RLock()
			if _, ok := p.clusters[clusterName]; ok {
				p.lock.RUnlock()
				return
			}
			p.lock.RUnlock()

			// slow path.
			p.lock.Lock()
			if _, ok := p.clusters[clusterName]; ok {
				p.lock.Unlock()
				return
			}

			// create new scoped cluster.
			clusterCtx, cancel := context.WithCancel(ctx)
			cl, err := newScopedCluster(p.config, clusterName, p.cache, p.scheme)
			if err != nil {
				p.log.Error(err, "failed to create cluster", "cluster", clusterName)
				cancel()
				p.lock.Unlock()
				return
			}
			p.clusters[clusterName] = cl
			p.cancelFns[clusterName] = cancel
			p.lock.Unlock()

			p.log.Info("engaging cluster", "cluster", clusterName)
			if err := mgr.Engage(clusterCtx, clusterName.String(), cl); err != nil {
				p.log.Error(err, "failed to engage cluster", "cluster", clusterName)
				p.lock.Lock()
				cancel()
				if p.clusters[clusterName] == cl {
					delete(p.clusters, clusterName)
					delete(p.cancelFns, clusterName)
				}
				p.lock.Unlock()
			}
		},
		DeleteFunc: func(obj interface{}) {
			lc, ok := obj.(*corev1alpha1.LogicalCluster)
			if !ok {
				tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("Couldn't get object from tombstone %#v", obj)
					return
				}
				lc, ok = tombstone.Obj.(*corev1alpha1.LogicalCluster)
				if !ok {
					klog.Errorf("Tombstone contained object that is not expected %#v", obj)
					return
				}
			}

			clusterName := logicalcluster.From(lc)

			p.lock.Lock()
			cancel, ok := p.cancelFns[clusterName]
			if ok {
				p.log.Info("disengaging cluster", "cluster", clusterName)
				cancel()
				delete(p.cancelFns, clusterName)
				delete(p.clusters, clusterName)
			}
			p.lock.Unlock()
		},
	})

	g.Go(func() error { return p.cache.Start(ctx) })

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !p.cache.WaitForCacheSync(syncCtx) {
		return fmt.Errorf("failed to sync wildcard cache")
	}

	return g.Wait()
}

// Get returns a cluster by name.
func (p *Provider) Get(_ context.Context, name string) (cluster.Cluster, error) {
	clusterName := logicalcluster.Name(name)

	p.lock.RLock()
	defer p.lock.RUnlock()
	if cl, ok := p.clusters[clusterName]; ok {
		return cl, nil
	}

	return nil, fmt.Errorf("cluster %q not found", name)
}
