package virtualworkspace

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kcp-dev/logicalcluster/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func newScopedCluster(cfg *rest.Config, clusterName logicalcluster.Name, wildcardCA WildcardCache, scheme *runtime.Scheme) (*scopedCluster, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + clusterName.Path().RequestPath()

	ca := &scopedCache{
		base:        wildcardCA,
		clusterName: clusterName,
		infGetter:   wildcardCA.getSharedInformer,
	}

	cli, err := client.New(cfg, client.Options{Cache: &client.CacheOptions{Reader: ca}})
	if err != nil {
		return nil, err
	}

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}

	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, err
	}

	return &scopedCluster{
		clusterName: clusterName,
		config:      cfg,
		scheme:      scheme,
		client:      cli,
		httpClient:  httpClient,
		mapper:      mapper,
		cache:       ca,
	}, nil
}

var _ cluster.Cluster = &scopedCluster{}

// scopedCluster is a cluster that operates on a specific namespace.
type scopedCluster struct {
	clusterName logicalcluster.Name

	scheme     *runtime.Scheme
	config     *rest.Config
	httpClient *http.Client
	client     client.Client
	mapper     meta.RESTMapper
	cache      cache.Cache
}

func (c *scopedCluster) GetHTTPClient() *http.Client {
	return c.httpClient
}

func (c *scopedCluster) GetConfig() *rest.Config {
	return c.config
}

func (c *scopedCluster) GetScheme() *runtime.Scheme {
	return c.scheme
}

func (c *scopedCluster) GetFieldIndexer() client.FieldIndexer {
	return c.cache
}

func (c *scopedCluster) GetRESTMapper() meta.RESTMapper {
	return c.mapper
}

// GetCache returns a cache.Cache.
func (c *scopedCluster) GetCache() cache.Cache {
	return c.cache
}

// GetClient returns a client scoped to the namespace.
func (c *scopedCluster) GetClient() client.Client {
	return c.client
}

// GetEventRecorderFor returns a new EventRecorder for the provided name.
func (c *scopedCluster) GetEventRecorderFor(name string) record.EventRecorder {
	panic("implement me")
}

// GetAPIReader returns a reader against the cluster.
func (c *scopedCluster) GetAPIReader() client.Reader {
	return c.GetAPIReader()
}

// Start starts the cluster.
func (c *scopedCluster) Start(ctx context.Context) error {
	return errors.New("scoped cluster cannot be started")
}
