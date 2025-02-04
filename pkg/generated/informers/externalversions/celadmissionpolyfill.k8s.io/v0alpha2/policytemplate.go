/*
Copyright The Kubernetes Authors.

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

// Code generated by informer-gen. DO NOT EDIT.

package v0alpha2

import (
	"context"
	time "time"

	celadmissionpolyfillk8siov0alpha2 "github.com/alexzielenski/cel_polyfill/pkg/apis/celadmissionpolyfill.k8s.io/v0alpha2"
	versioned "github.com/alexzielenski/cel_polyfill/pkg/generated/clientset/versioned"
	internalinterfaces "github.com/alexzielenski/cel_polyfill/pkg/generated/informers/externalversions/internalinterfaces"
	v0alpha2 "github.com/alexzielenski/cel_polyfill/pkg/generated/listers/celadmissionpolyfill.k8s.io/v0alpha2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
)

// PolicyTemplateInformer provides access to a shared informer and lister for
// PolicyTemplates.
type PolicyTemplateInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v0alpha2.PolicyTemplateLister
}

type policyTemplateInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewPolicyTemplateInformer constructs a new informer for PolicyTemplate type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewPolicyTemplateInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredPolicyTemplateInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredPolicyTemplateInformer constructs a new informer for PolicyTemplate type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredPolicyTemplateInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CeladmissionpolyfillV0alpha2().PolicyTemplates(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CeladmissionpolyfillV0alpha2().PolicyTemplates(namespace).Watch(context.TODO(), options)
			},
		},
		&celadmissionpolyfillk8siov0alpha2.PolicyTemplate{},
		resyncPeriod,
		indexers,
	)
}

func (f *policyTemplateInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredPolicyTemplateInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *policyTemplateInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&celadmissionpolyfillk8siov0alpha2.PolicyTemplate{}, f.defaultInformer)
}

func (f *policyTemplateInformer) Lister() v0alpha2.PolicyTemplateLister {
	return v0alpha2.NewPolicyTemplateLister(f.Informer().GetIndexer())
}
