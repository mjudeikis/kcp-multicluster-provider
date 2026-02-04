/*
Copyright 2018 The Kubernetes Authors.

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

package recorder

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	eventsv1client "k8s.io/client-go/kubernetes/typed/events/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
)

// Forked: sigs.k8s.io/controller-runtime@v0.23.0/pkg/internal/recorder/recorder.go
// Forking notes:
// 1. stopBroadcaster, stop and events.EventBroadcaster moved to dedicated broadcaster struct to allow multiple broadcasters
// 2. typed client replaced with http.Client to allow sharing clients across multiple broadcasters and typed client will be created in getBroadcaster
// 2. Remove deprecated broadcaster usage early here.
// 3. Stop function modified to stop all broadcasters.
// 4. Change signature for getBroadcaster to return addtional error
// 5. GetEventRecorderFor marked as deprecated and new GetEventRecorder added
// 6. lazyRecorder modified to support new GetEventRecorder signature

// var _ recorder.Provider = &Provider{}

// EventBroadcasterProducer makes an event broadcaster, returning
// whether or not the broadcaster should be stopped with the Provider,
// or not (e.g. if it's shared, it shouldn't be stopped with the Provider).
// This producer currently produces both an old API and a new API broadcaster.
type EventBroadcasterProducer func(*eventsv1client.EventsV1Client) (deprecatedCaster record.EventBroadcaster, caster events.EventBroadcaster, stopWithProvider bool)

// Provider is a recorder.Provider that records events to the k8s API server
// and to a logr Logger.
type Provider struct {
	lock sync.RWMutex

	httpClient *http.Client
	// scheme to specify when creating a recorder
	scheme *runtime.Scheme
	// logger is the logger to use when logging diagnostic event info
	logger          logr.Logger
	makeBroadcaster EventBroadcasterProducer

	broadcasters map[string]*broadcasterWrapper
}

type broadcasterWrapper struct {
	deprecatedBroadcaster record.EventBroadcaster
	stopBroadcaster       bool

	broadcaster             events.EventBroadcaster
	cancelSinkRecordingFunc context.CancelFunc
	stopWatcherFunc         func()

	stopped bool
}

// NB(directxman12): this manually implements Stop instead of Being a runnable because we need to
// stop it *after* everything else shuts down, otherwise we'll cause panics as the leader election
// code finishes up and tries to continue emitting events.

// Stop attempts to stop this provider, stopping the underlying broadcaster
// if the broadcaster asked to be stopped.  It kinda tries to honor the given
// context, but the underlying broadcaster has an indefinite wait that doesn't
// return until all queued events are flushed, so this may end up just returning
// before the underlying wait has finished instead of cancelling the wait.
// This is Very Frustrating™.
func (p *Provider) Stop(shutdownCtx context.Context) {
	doneCh := make(chan struct{})

	go func() {
		// technically, this could start the broadcaster, but practically, it's
		// almost certainly already been started (e.g. by leader election).  We
		// need to invoke this to ensure that we don't inadvertently race with
		// an invocation of getBroadcaster.
		for key := range p.broadcasters {
			wrappedBroadcaster, err := p.getBroadcaster(nil, key)
			if err == nil && p.broadcasters[key].stopBroadcaster {
				p.lock.Lock()
				wrappedBroadcaster.broadcaster.Shutdown()
				wrappedBroadcaster.cancelSinkRecordingFunc()
				wrappedBroadcaster.stopWatcherFunc()
				wrappedBroadcaster.deprecatedBroadcaster.Shutdown()
				p.broadcasters[key].stopped = true
				p.lock.Unlock()
			}
		}
		close(doneCh)
	}()

	select {
	case <-shutdownCtx.Done():
	case <-doneCh:
	}
}

// getBroadcaster ensures that a broadcaster is started for this
// provider, and returns it.  It's threadsafe.
func (p *Provider) getBroadcaster(cfg *rest.Config, clusterName string) (*broadcasterWrapper, error) {
	// NB(directxman12): this can technically still leak if something calls
	// "getBroadcaster" (i.e. Emits an Event) but never calls Start, but if we
	// create the broadcaster in start, we could race with other things that
	// are started at the same time & want to emit events.  The alternative is
	// silently swallowing events and more locking, but that seems suboptimal.
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.broadcasters == nil {
		p.broadcasters = make(map[string]*broadcasterWrapper)
	}

	if _, ok := p.broadcasters[clusterName]; !ok {
		if cfg == nil { // we need a config to create a new broadcaster
			return nil, fmt.Errorf("config must be provided for new broadcaster for cluster %s", clusterName)
		}
		corev1Client, err := corev1client.NewForConfigAndClient(cfg, p.httpClient)
		if err != nil {
			return nil, fmt.Errorf("failed to init core v1 client for event broadcaster: %w", err)
		}
		eventsv1Client, err := eventsv1client.NewForConfigAndClient(cfg, p.httpClient)
		if err != nil {
			return nil, fmt.Errorf("failed to init events v1 client for event broadcaster: %w", err)
		}
		deprecatedBroadcaster, broadcaster, stopBroadcaster := p.makeBroadcaster(eventsv1Client)

		// init deprecated broadcaster
		deprecatedBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: corev1Client.Events("")})
		deprecatedBroadcaster.StartEventWatcher(
			func(e *corev1.Event) {
				p.logger.V(1).Info(e.Message, "type", e.Type, "object", e.InvolvedObject, "reason", e.Reason, "clusterName", clusterName)
			})

		// init new broadcaster
		ctx, cancel := context.WithCancel(context.Background())
		if err := broadcaster.StartRecordingToSinkWithContext(ctx); err != nil {
			p.logger.Error(err, "error starting recording for broadcaster")
			return nil, err
		}

		stopWatcher, err := broadcaster.StartEventWatcher(func(event runtime.Object) {
			e, isEvt := event.(*eventsv1.Event)
			if isEvt {
				p.logger.V(1).Info(e.Note, "type", e.Type, "object", e.Related, "action", e.Action, "reason", e.Reason)
			}
		})
		if err != nil {
			p.logger.Error(err, "error starting event watcher for broadcaster")
		}

		p.broadcasters[clusterName] = &broadcasterWrapper{
			broadcaster:             broadcaster,
			stopBroadcaster:         stopBroadcaster,
			deprecatedBroadcaster:   deprecatedBroadcaster,
			cancelSinkRecordingFunc: cancel,
			stopWatcherFunc:         stopWatcher,
		}
	}

	return p.broadcasters[clusterName], nil
}

// NewProvider create a new Provider instance.
func NewProvider(httpClient *http.Client, scheme *runtime.Scheme, logger logr.Logger, makeBroadcaster EventBroadcasterProducer) (*Provider, error) {
	if httpClient == nil {
		panic("httpClient must not be nil")
	}

	p := &Provider{scheme: scheme, logger: logger, makeBroadcaster: makeBroadcaster, httpClient: httpClient}
	return p, nil
}

// GetEventRecorderFor returns an event recorder that broadcasts to this provider's
// broadcaster.  All events will be associated with a component of the given name.
func (p *Provider) GetEventRecorderFor(cfg *rest.Config, name, clusterName string) record.EventRecorder {
	return &deprecatedRecorder{
		prov:        p,
		name:        name,
		clusterName: clusterName,
		cfg:         cfg,
	}
}

// GetEventRecorder returns an event recorder that broadcasts to this provider's
// broadcaster.  All events will be associated with a component of the given name.
func (p *Provider) GetEventRecorder(cfg *rest.Config, name, clusterName string) events.EventRecorder {
	return &lazyRecorder{
		prov:        p,
		name:        name,
		clusterName: clusterName,
		cfg:         cfg,
	}
}

// lazyRecorder is a recorder that doesn't actually instantiate any underlying
// recorder until the first event is emitted.
type lazyRecorder struct {
	prov *Provider
	name string

	recOnce sync.Once
	rec     events.EventRecorder

	// kcp specific:
	clusterName string
	cfg         *rest.Config
}

// ensureRecording ensures that a concrete recorder is populated for this recorder.
func (l *lazyRecorder) ensureRecording() {
	l.recOnce.Do(func() {
		wrappedBroadcaster, err := l.prov.getBroadcaster(nil, l.clusterName)
		if err != nil {
			l.prov.logger.Error(err, "error getting broadcaster for cluster", "cluster", l.clusterName)
			return
		}
		l.rec = wrappedBroadcaster.broadcaster.NewRecorder(l.prov.scheme, l.name)
	})
}

func (l *lazyRecorder) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any) {
	l.ensureRecording()

	l.prov.lock.RLock()
	if !l.prov.broadcasters[l.clusterName].stopped && l.rec != nil {
		l.rec.Eventf(regarding, related, eventtype, reason, action, note, args...)
	}
	l.prov.lock.RUnlock()
}

// deprecatedRecorder implements the old events API during the tranisiton and will be removed in a future release.
//
// Deprecated: will be removed in a future release.
type deprecatedRecorder struct {
	prov *Provider
	name string

	recOnce sync.Once
	rec     record.EventRecorder

	cfg         *rest.Config
	clusterName string
}

// ensureRecording ensures that a concrete recorder is populated for this recorder.
func (l *deprecatedRecorder) ensureRecording() {
	l.recOnce.Do(func() {
		wrappedBroadcaster, err := l.prov.getBroadcaster(nil, l.clusterName)
		if err != nil {
			l.prov.logger.Error(err, "error getting broadcaster for cluster", "cluster", l.clusterName)
			return
		}
		l.rec = wrappedBroadcaster.deprecatedBroadcaster.NewRecorder(l.prov.scheme, corev1.EventSource{Component: l.name})
	})
}

func (l *deprecatedRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	l.ensureRecording()

	l.prov.lock.RLock()
	if !l.prov.broadcasters[l.clusterName].stopped {
		l.rec.Event(object, eventtype, reason, message)
	}
	l.prov.lock.RUnlock()
}

func (l *deprecatedRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any) {
	l.ensureRecording()

	l.prov.lock.RLock()
	if l.prov.broadcasters[l.clusterName] != nil && !l.prov.broadcasters[l.clusterName].stopped {
		l.rec.Eventf(object, eventtype, reason, messageFmt, args...)
	}
	l.prov.lock.RUnlock()
}

func (l *deprecatedRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...any) {
	l.ensureRecording()

	l.prov.lock.RLock()
	if !l.prov.broadcasters[l.clusterName].stopped {
		l.rec.AnnotatedEventf(object, annotations, eventtype, reason, messageFmt, args...)
	}
	l.prov.lock.RUnlock()
}

// StopBroadcaster shuts down an individual broadcaster for a specific logical cluster name
func (p *Provider) StopBroadcaster(clusterName string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if caster, ok := p.broadcasters[clusterName]; ok {
		caster.broadcaster.Shutdown()
		caster.cancelSinkRecordingFunc()
		caster.stopWatcherFunc()
		caster.deprecatedBroadcaster.Shutdown()
		caster.stopped = true
		delete(p.broadcasters, clusterName)
	}

	return nil
}
