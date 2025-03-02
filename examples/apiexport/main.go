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

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"

	apisv1alpha1 "github.com/kcp-dev/kcp/sdk/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/kcp/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/sdk/apis/tenancy/v1alpha1"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcbuilder "github.com/multicluster-runtime/multicluster-runtime/pkg/builder"
	mcmanager "github.com/multicluster-runtime/multicluster-runtime/pkg/manager"
	mcreconcile "github.com/multicluster-runtime/multicluster-runtime/pkg/reconcile"

	"github.com/kcp-dev/multicluster-runtime-provider/virtualworkspace"
)

func init() {
	runtime.Must(corev1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(tenancyv1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(apisv1alpha1.AddToScheme(scheme.Scheme))
}

func main() {
	log.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx := signals.SetupSignalHandler()
	entryLog := log.Log.WithName("entrypoint")

	var (
		server   string
		provider *virtualworkspace.Provider
	)

	pflag.StringVar(&server, "server", "", "Override for kubeconfig server URL")
	pflag.Parse()

	cfg := ctrl.GetConfigOrDie()
	cfg = rest.CopyConfig(cfg)

	if server != "" {
		cfg.Host = server
	}

	// Setup a Manager, note that this not yet engages clusters, only makes them available.
	entryLog.Info("Setting up manager")
	opts := manager.Options{}

	var err error
	provider, err = virtualworkspace.New(cfg, &apisv1alpha1.APIBinding{}, virtualworkspace.Options{})
	if err != nil {
		entryLog.Error(err, "unable to construct cluster provider")
		os.Exit(1)
	}

	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			fmt.Println(r.URL)
			return rt.RoundTrip(r)
		})
	}

	mgr, err := mcmanager.New(cfg, provider, opts)
	if err != nil {
		entryLog.Error(err, "unable to set up overall controller manager")
		os.Exit(1)
	}

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("kcp-secret-controller").
		Watches(&corev1.Secret{}, virtualworkspace.EventHandlerFunc).
		Complete(mcreconcile.Func(
			func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
				log := log.FromContext(ctx).WithValues("cluster", req.ClusterName)

				cl, err := mgr.GetCluster(ctx, req.ClusterName)
				if err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to get cluster: %w", err)
				}
				client := cl.GetClient()

				// Retrieve the Secret from the cluster.
				secret := &corev1.Secret{}
				if err := client.Get(ctx, req.NamespacedName, secret); err != nil {
					log.Error(err, "did not find secret")
					if !apierrors.IsNotFound(err) {
						return reconcile.Result{}, fmt.Errorf("failed to get secret: %w", err)
					}
					// Secret was deleted.
					return reconcile.Result{}, nil
				}

				// If the Secret is being deleted, we can skip it.
				if secret.DeletionTimestamp != nil {
					return reconcile.Result{}, nil
				}

				log.Info("Reconciling Secret", "ns", secret.GetNamespace(), "name", secret.Name, "uuid", secret.UID)

				secrets := &corev1.SecretList{}
				if err := client.List(ctx, secrets); err != nil {
					log.Error(err, "failed to list secrets in same cluster")
					return reconcile.Result{}, err
				}

				cm := &corev1.ConfigMap{
					ObjectMeta: v1.ObjectMeta{
						Name:      req.Name,
						Namespace: req.Namespace,
					},
				}

				res, err := ctrl.CreateOrUpdate(ctx, client, cm, func() error {
					if cm.Data == nil {
						cm.Data = make(map[string]string)
					}

					cm.Data["secrets"] = strconv.Itoa(len(secrets.Items))

					return nil
				})
				if err != nil {
					return reconcile.Result{}, err
				}

				log.Info("Reconciled child ConfigMap", "result", res)

				return reconcile.Result{}, nil
			},
		)); err != nil {
		entryLog.Error(err, "failed to build controller")
		os.Exit(1)
	}

	if provider != nil {
		entryLog.Info("Starting provider")
		go func() {
			if err := provider.Run(ctx, mgr); err != nil {
				entryLog.Error(err, "unable to run provider")
				os.Exit(1)
			}
		}()
	}

	entryLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		entryLog.Error(err, "unable to run manager")
		os.Exit(1)
	}
}

type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
