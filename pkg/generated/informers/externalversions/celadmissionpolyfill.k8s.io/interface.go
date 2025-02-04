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

package celadmissionpolyfill

import (
	v0alpha1 "github.com/alexzielenski/cel_polyfill/pkg/generated/informers/externalversions/celadmissionpolyfill.k8s.io/v0alpha1"
	v0alpha2 "github.com/alexzielenski/cel_polyfill/pkg/generated/informers/externalversions/celadmissionpolyfill.k8s.io/v0alpha2"
	internalinterfaces "github.com/alexzielenski/cel_polyfill/pkg/generated/informers/externalversions/internalinterfaces"
)

// Interface provides access to each of this group's versions.
type Interface interface {
	// V0alpha1 provides access to shared informers for resources in V0alpha1.
	V0alpha1() v0alpha1.Interface
	// V0alpha2 provides access to shared informers for resources in V0alpha2.
	V0alpha2() v0alpha2.Interface
}

type group struct {
	factory          internalinterfaces.SharedInformerFactory
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &group{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// V0alpha1 returns a new v0alpha1.Interface.
func (g *group) V0alpha1() v0alpha1.Interface {
	return v0alpha1.New(g.factory, g.namespace, g.tweakListOptions)
}

// V0alpha2 returns a new v0alpha2.Interface.
func (g *group) V0alpha2() v0alpha2.Interface {
	return v0alpha2.New(g.factory, g.namespace, g.tweakListOptions)
}
