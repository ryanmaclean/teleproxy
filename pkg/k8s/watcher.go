package k8s

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	pwatch "k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
)

type empty struct{}

type listWatchAdapter struct {
	resource dynamic.ResourceInterface
}

func (lw listWatchAdapter) List(options v1.ListOptions) (runtime.Object, error) {
	// silently coerce the returned *unstructured.UnstructuredList
	// struct to a runtime.Object interface.
	return lw.resource.List(options)
}

func (lw listWatchAdapter) Watch(options v1.ListOptions) (pwatch.Interface, error) {
	return lw.resource.Watch(options)
}

type Watcher struct {
	client  *Client
	watches map[string]watch
	mutex   sync.Mutex
	started bool

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type watch struct {
	namespace string
	resource  dynamic.NamespaceableResourceInterface
	store     cache.Store
	invoke    func()
	runner    func()
}

// NewWatcher returns a Kubernetes Watcher for the specified cluster
func NewWatcher(c *Client) *Watcher {
	w := &Watcher{
		client:  c,
		watches: make(map[string]watch),
		stopCh:  make(chan struct{}),
	}

	return w
}

func (w *Watcher) Watch(resources string, listener func(*Watcher)) error {
	return w.WatchNamespace("", resources, listener)
}

func (w *Watcher) WatchNamespace(namespace, resources string, listener func(*Watcher)) error {
	ri := w.client.resolve(resources)
	dyn, err := dynamic.NewForConfig(w.client.config)
	if err != nil {
		log.Fatal(err)
	}

	resource := dyn.Resource(schema.GroupVersionResource{
		Group:    ri.Group,
		Version:  ri.Version,
		Resource: ri.Name,
	})
	var watched dynamic.ResourceInterface
	if namespace != "" {
		watched = resource.Namespace(namespace)
	} else {
		watched = resource
	}

	invoke := func() {
		w.mutex.Lock()
		defer w.mutex.Unlock()
		listener(w)
	}

	store, controller := cache.NewInformer(
		listWatchAdapter{watched},
		nil,
		5*time.Minute,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				invoke()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldUn := oldObj.(*unstructured.Unstructured)
				newUn := newObj.(*unstructured.Unstructured)
				// we ignore updates for objects
				// already in our store because we
				// assume this means we made the
				// change to them
				if oldUn.GetResourceVersion() != newUn.GetResourceVersion() {
					invoke()
				}
			},
			DeleteFunc: func(obj interface{}) {
				invoke()
			},
		},
	)

	runner := func() {
		controller.Run(w.stopCh)
		w.wg.Done()
	}

	kind := w.client.Canonicalize(ri.Kind)
	w.watches[kind] = watch{
		namespace: namespace,
		resource:  resource,
		store:     store,
		invoke:    invoke,
		runner:    runner,
	}

	return nil
}

func (w *Watcher) Start() {
	w.mutex.Lock()
	if w.started {
		w.mutex.Unlock()
		return
	} else {
		w.started = true
		w.mutex.Unlock()
	}
	for kind := range w.watches {
		w.sync(kind)
	}

	for _, watch := range w.watches {
		watch.invoke()
	}

	w.wg.Add(len(w.watches))
	for _, watch := range w.watches {
		go watch.runner()
	}
}

func (w *Watcher) sync(kind string) {
	watch := w.watches[kind]
	resources, err := w.client.List(kind)
	if err != nil {
		log.Fatal(err)
	}
	for _, rsrc := range resources {
		var uns unstructured.Unstructured
		uns.SetUnstructuredContent(rsrc)
		err = watch.store.Update(&uns)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func (w *Watcher) List(kind string) []Resource {
	kind = w.client.Canonicalize(kind)
	watch, ok := w.watches[kind]
	if ok {
		objs := watch.store.List()
		result := make([]Resource, len(objs))
		for idx, obj := range objs {
			result[idx] = obj.(*unstructured.Unstructured).UnstructuredContent()
		}
		return result
	} else {
		return nil
	}
}

func (w *Watcher) UpdateStatus(resource Resource) (Resource, error) {
	kind := w.client.Canonicalize(resource.Kind())
	if kind == "" {
		return nil, fmt.Errorf("unknow resource: %v", resource.Kind())
	}
	watch, ok := w.watches[kind]
	if !ok {
		return nil, fmt.Errorf("no watch: %s", kind)
	}

	var uns unstructured.Unstructured
	uns.SetUnstructuredContent(resource)

	// XXX: should we have an if Namespaced here?
	result, err := watch.resource.Namespace(uns.GetNamespace()).UpdateStatus(&uns, v1.UpdateOptions{})
	if err != nil {
		return nil, err
	} else {
		watch.store.Update(result)
		return result.UnstructuredContent(), nil
	}
}

func (w *Watcher) Get(kind, qname string) Resource {
	resources := w.List(kind)
	for _, res := range resources {
		if strings.ToLower(res.QName()) == strings.ToLower(qname) {
			return res
		}
	}
	return Resource{}
}

func (w *Watcher) Exists(kind, qname string) bool {
	return w.Get(kind, qname).Name() != ""
}

func (w *Watcher) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

func (w *Watcher) Wait() {
	w.Start()
	w.wg.Wait()
}
