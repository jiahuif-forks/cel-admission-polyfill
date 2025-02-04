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

// Code generated by client-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	"time"

	v1alpha1 "github.com/alexzielenski/cel_polyfill/pkg/apis/admissionregistration.polyfill.sigs.k8s.io/v1alpha1"
	scheme "github.com/alexzielenski/cel_polyfill/pkg/generated/clientset/versioned/scheme"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// ValidatingAdmissionPoliciesGetter has a method to return a ValidatingAdmissionPolicyInterface.
// A group's client should implement this interface.
type ValidatingAdmissionPoliciesGetter interface {
	ValidatingAdmissionPolicies() ValidatingAdmissionPolicyInterface
}

// ValidatingAdmissionPolicyInterface has methods to work with ValidatingAdmissionPolicy resources.
type ValidatingAdmissionPolicyInterface interface {
	Create(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.CreateOptions) (*v1alpha1.ValidatingAdmissionPolicy, error)
	Update(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.UpdateOptions) (*v1alpha1.ValidatingAdmissionPolicy, error)
	UpdateStatus(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.UpdateOptions) (*v1alpha1.ValidatingAdmissionPolicy, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*v1alpha1.ValidatingAdmissionPolicy, error)
	List(ctx context.Context, opts v1.ListOptions) (*v1alpha1.ValidatingAdmissionPolicyList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.ValidatingAdmissionPolicy, err error)
	ValidatingAdmissionPolicyExpansion
}

// validatingAdmissionPolicies implements ValidatingAdmissionPolicyInterface
type validatingAdmissionPolicies struct {
	client rest.Interface
}

// newValidatingAdmissionPolicies returns a ValidatingAdmissionPolicies
func newValidatingAdmissionPolicies(c *AdmissionregistrationV1alpha1Client) *validatingAdmissionPolicies {
	return &validatingAdmissionPolicies{
		client: c.RESTClient(),
	}
}

// Get takes name of the validatingAdmissionPolicy, and returns the corresponding validatingAdmissionPolicy object, and an error if there is any.
func (c *validatingAdmissionPolicies) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.ValidatingAdmissionPolicy, err error) {
	result = &v1alpha1.ValidatingAdmissionPolicy{}
	err = c.client.Get().
		Resource("validatingadmissionpolicies").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do(ctx).
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of ValidatingAdmissionPolicies that match those selectors.
func (c *validatingAdmissionPolicies) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.ValidatingAdmissionPolicyList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1alpha1.ValidatingAdmissionPolicyList{}
	err = c.client.Get().
		Resource("validatingadmissionpolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx).
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested validatingAdmissionPolicies.
func (c *validatingAdmissionPolicies) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Resource("validatingadmissionpolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch(ctx)
}

// Create takes the representation of a validatingAdmissionPolicy and creates it.  Returns the server's representation of the validatingAdmissionPolicy, and an error, if there is any.
func (c *validatingAdmissionPolicies) Create(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.CreateOptions) (result *v1alpha1.ValidatingAdmissionPolicy, err error) {
	result = &v1alpha1.ValidatingAdmissionPolicy{}
	err = c.client.Post().
		Resource("validatingadmissionpolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(validatingAdmissionPolicy).
		Do(ctx).
		Into(result)
	return
}

// Update takes the representation of a validatingAdmissionPolicy and updates it. Returns the server's representation of the validatingAdmissionPolicy, and an error, if there is any.
func (c *validatingAdmissionPolicies) Update(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.UpdateOptions) (result *v1alpha1.ValidatingAdmissionPolicy, err error) {
	result = &v1alpha1.ValidatingAdmissionPolicy{}
	err = c.client.Put().
		Resource("validatingadmissionpolicies").
		Name(validatingAdmissionPolicy.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(validatingAdmissionPolicy).
		Do(ctx).
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *validatingAdmissionPolicies) UpdateStatus(ctx context.Context, validatingAdmissionPolicy *v1alpha1.ValidatingAdmissionPolicy, opts v1.UpdateOptions) (result *v1alpha1.ValidatingAdmissionPolicy, err error) {
	result = &v1alpha1.ValidatingAdmissionPolicy{}
	err = c.client.Put().
		Resource("validatingadmissionpolicies").
		Name(validatingAdmissionPolicy.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(validatingAdmissionPolicy).
		Do(ctx).
		Into(result)
	return
}

// Delete takes name of the validatingAdmissionPolicy and deletes it. Returns an error if one occurs.
func (c *validatingAdmissionPolicies) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	return c.client.Delete().
		Resource("validatingadmissionpolicies").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *validatingAdmissionPolicies) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	var timeout time.Duration
	if listOpts.TimeoutSeconds != nil {
		timeout = time.Duration(*listOpts.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Resource("validatingadmissionpolicies").
		VersionedParams(&listOpts, scheme.ParameterCodec).
		Timeout(timeout).
		Body(&opts).
		Do(ctx).
		Error()
}

// Patch applies the patch and returns the patched validatingAdmissionPolicy.
func (c *validatingAdmissionPolicies) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.ValidatingAdmissionPolicy, err error) {
	result = &v1alpha1.ValidatingAdmissionPolicy{}
	err = c.client.Patch(pt).
		Resource("validatingadmissionpolicies").
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
