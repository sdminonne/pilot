// Copyright 2016 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"istio.io/manager/model"

	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/runtime"
	"k8s.io/client-go/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// Controller is a collection of synchronized resource watchers
// Caches are thread-safe
type Controller struct {
	client *Client
	queue  Queue

	kinds     map[string]cacheHandler
	endpoints cacheHandler
	services  cacheHandler
}

type cacheHandler struct {
	informer cache.SharedIndexInformer
	handler  *chainHandler
}

// NewController creates a new Kubernetes controller
func NewController(
	client *Client,
	namespace string,
	resyncPeriod time.Duration,
) *Controller {
	// Queue requires a time duration for a retry delay after a handler error
	out := &Controller{
		client: client,
		queue:  NewQueue(1 * time.Second),
		kinds:  make(map[string]cacheHandler),
	}

	out.services = out.createInformer(&v1.Service{}, resyncPeriod,
		func(opts v1.ListOptions) (runtime.Object, error) {
			return client.client.Services(namespace).List(opts)
		},
		func(opts v1.ListOptions) (watch.Interface, error) {
			return client.client.Services(namespace).Watch(opts)
		})

	out.endpoints = out.createInformer(&v1.Endpoints{}, resyncPeriod,
		func(opts v1.ListOptions) (runtime.Object, error) {
			return client.client.Endpoints(namespace).List(opts)
		},
		func(opts v1.ListOptions) (watch.Interface, error) {
			return client.client.Endpoints(namespace).Watch(opts)
		})

	// add stores for TRP kinds
	for kind := range client.mapping {
		out.kinds[kind] = out.createInformer(&Config{}, resyncPeriod,
			func(opts v1.ListOptions) (result runtime.Object, err error) {
				result = &ConfigList{}
				err = client.dyn.Get().
					Namespace(namespace).
					Resource(kind+"s").
					VersionedParams(&opts, api.ParameterCodec).
					Do().
					Into(result)
				return
			},
			func(opts v1.ListOptions) (watch.Interface, error) {
				return client.dyn.Get().
					Prefix("watch").
					Namespace(namespace).
					Resource(kind+"s").
					VersionedParams(&opts, api.ParameterCodec).
					Watch()
			})
	}

	return out
}

func (c *Controller) notify(obj interface{}, event int) error {
	if !c.HasSynced() {
		return errors.New("Waiting till full synchronization")
	}
	k, _ := keyFunc(obj)
	log.Printf("%s: %#v", eventString(event), k)
	return nil
}

func (c *Controller) createInformer(
	o runtime.Object,
	resyncPeriod time.Duration,
	lf cache.ListFunc,
	wf cache.WatchFunc) cacheHandler {
	handler := &chainHandler{funcs: []Handler{c.notify}}

	// TODO: finer-grained index (perf)
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{ListFunc: lf, WatchFunc: wf}, o,
		resyncPeriod, cache.Indexers{})

	err := informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			// TODO: filtering functions to skip over un-referenced resources (perf)
			AddFunc: func(obj interface{}) {
				c.queue.Push(Task{handler: handler.apply, obj: obj, event: evAdd})
			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					c.queue.Push(Task{handler: handler.apply, obj: cur, event: evUpdate})
				}
			},
			DeleteFunc: func(obj interface{}) {
				c.queue.Push(Task{handler: handler.apply, obj: obj, event: evDelete})
			},
		})
	if err != nil {
		log.Print(err)
	}

	return cacheHandler{informer: informer, handler: handler}
}

// AppendHandler adds a handler for a config resource.
// Handler executes on the single worker queue.
// Cache view is as AT LEAST as fresh as the moment notification arrives, but
// MAY BE more fresh (e.g. "delete" cancels "add" event in the cache).
// Note: this method is not thread-safe, please use it before calling Run
func (c *Controller) AppendHandler(
	kind string,
	f func(*model.Config, int) error) error {
	ch, ok := c.kinds[kind]
	if !ok {
		return fmt.Errorf("Cannot locate kind %q", kind)
	}
	ch.handler.append(func(obj interface{}, ev int) error {
		cfg, err := kubeToModel(kind, c.client.mapping[kind], obj.(*Config))
		if err == nil {
			return f(cfg, ev)
		}
		log.Printf("Cannot convert TRP of kind %s to config object", kind)
		return nil
	})
	return nil
}

// HasSynced returns true after the initial state synchronization
func (c *Controller) HasSynced() bool {
	if !c.services.informer.HasSynced() || !c.endpoints.informer.HasSynced() {
		return false
	}
	for kind, ctl := range c.kinds {
		if !ctl.informer.HasSynced() {
			log.Printf("Controller %q is syncing...", kind)
			return false
		}
	}
	return true
}

// Run all controllers until a signal is received
func (c *Controller) Run(stop chan struct{}) {
	go c.queue.Run(stop)
	go c.services.informer.Run(stop)
	go c.endpoints.informer.Run(stop)
	for _, ctl := range c.kinds {
		go ctl.informer.Run(stop)
	}
	<-stop
}

// key function used internally by kubernetes
// Typically, key is a string "namespace"/"name"
func keyFunc(obj interface{}) (string, bool) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("Creating key failed: %v", err)
		return k, false
	}
	return k, true
}

func (c *Controller) Get(key model.ConfigKey) (*model.Config, bool) {
	if err := c.client.mapping.ValidateKey(&key); err != nil {
		log.Print(err)
		return nil, false
	}

	store := c.kinds[key.Kind].informer.GetStore()
	data, exists, err := store.GetByKey(key.Namespace + "/" + key.Name)
	if !exists {
		return nil, false
	}
	if err != nil {
		log.Print(err)
		return nil, false
	}
	out, err := kubeToModel(key.Kind, c.client.mapping[key.Kind], data.(*Config))
	if err != nil {
		log.Print(err)
		return nil, false
	}
	return out, true
}

// Put applies operation to the remote storage ONLY
// This implies that you might not see the effect immediately
func (c *Controller) Put(obj *model.Config) error {
	return c.client.Put(obj)
}

// Delete applies operation to the remote storage ONLY
// This implies that you might not see the effect immediately
func (c *Controller) Delete(key model.ConfigKey) error {
	return c.client.Delete(key)
}

func (c *Controller) List(kind string, ns string) []*model.Config {
	if _, ok := c.kinds[kind]; !ok {
		return nil
	}

	// TODO: use indexed cache
	var out []*model.Config
	for _, data := range c.kinds[kind].informer.GetStore().List() {
		config := data.(*Config)
		if config.Metadata.Namespace == ns {
			elt, err := kubeToModel(kind, c.client.mapping[kind], data.(*Config))
			if err != nil {
				log.Print(err)
			} else {
				out = append(out, elt)
			}
		}
	}
	return out
}

const (
	// Object is added
	evAdd = 1
	// Object is modified. Called when a re-list happens.
	evUpdate = 2
	// Object is deleted. Captures the object at the last state known, or
	// potentially an object of type DeletedFinalStateUnknown
	evDelete = 3
)

func eventString(event int) string {
	eventType := "unknown"
	switch event {
	case evAdd:
		eventType = "Add"
	case evUpdate:
		eventType = "Update"
	case evDelete:
		eventType = "Delete"
	}
	return eventType
}