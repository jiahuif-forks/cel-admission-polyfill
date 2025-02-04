/*
Copyright 2022 The Kubernetes Authors.

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

package cel

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexzielenski/cel_polyfill"
	"github.com/alexzielenski/cel_polyfill/pkg/controller/admissionregistration.polyfill.sigs.k8s.io/v1alpha1"
	crdv1alpha1 "github.com/alexzielenski/cel_polyfill/pkg/controller/admissionregistration.polyfill.sigs.k8s.io/v1alpha1"
	"github.com/alexzielenski/cel_polyfill/pkg/controller/schemaresolver"
	"github.com/alexzielenski/cel_polyfill/pkg/generated/clientset/versioned"
	"github.com/alexzielenski/cel_polyfill/pkg/webhook"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"

	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
)

type KI = kubernetes.Interface
type APIEXTI = apiextensionsclientset.Interface
type DYNI = dynamic.Interface

type testClientInterface interface {
	KI
	APIEXTI
	DYNI
}

type testClient struct {
	KI
	APIEXTI
	DYNI
}

func (tc testClient) Discovery() discovery.DiscoveryInterface {
	return tc.KI.Discovery()
}

var webhookServer webhook.Interface
var webhookValidator *swapValidator = &swapValidator{}

type swapValidator struct {
	current atomic.Pointer[admission.ValidationInterface]
}

func (s *swapValidator) Set(v admission.ValidationInterface) {
	s.current.Store(&v)
}

func (s *swapValidator) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	cur := s.current.Load()
	if cur == nil {
		return nil
	}
	return (*cur).Validate(ctx, a, o)
}

func (s *swapValidator) Handles(operation admission.Operation) bool {
	cur := s.current.Load()
	if cur == nil {
		return false
	}
	return (*cur).Handles(operation)
}

type noopValidator struct {
}

func (s noopValidator) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	return nil
}

func (s noopValidator) Handles(operation admission.Operation) bool {
	return false
}

func serverScope(transformConfig func(*rest.Config)) (testClientInterface, func()) {
	if webhookValidator != nil {
		webhookValidator.Set(noopValidator{})
	}

	ctx, cancel := context.WithCancel(context.Background())
	// User expected to just have a k8s cluster running. We will try to reuse it
	// for all tests, and wipe out anything added in between tests.
	// Connect to k8s
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// if you want to change the loading rules (which files in which order), you can do so here

	configOverrides := &clientcmd.ConfigOverrides{}
	// if you want to change override values or bind them to flags, there are methods to help you

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		panic(err)
	}

	NewTestClient := func(config *rest.Config) testClient {
		kube := kubernetes.NewForConfigOrDie(config)
		apiext := apiextensionsclientset.NewForConfigOrDie(config)
		custom := versioned.NewForConfigOrDie(config)
		dyn := dynamic.NewForConfigOrDie(config)

		client := testClient{
			KI:      crdv1alpha1.NewWrappedClient(kube, custom),
			APIEXTI: apiext,
			DYNI:    dyn,
		}

		return client
	}

	client := NewTestClient(config)

	factory := informers.NewSharedInformerFactory(client, 30*time.Second)
	apiextensionsFactory := apiextensionsinformers.NewSharedInformerFactory(client, 30*time.Second)
	restmapper := meta.NewLazyRESTMapperLoader(func() (meta.RESTMapper, error) {
		groupResources, err := restmapper.GetAPIGroupResources(client.Discovery())
		if err != nil {
			return nil, err
		}
		return restmapper.NewDiscoveryRESTMapper(groupResources), nil
	}).(meta.ResettableRESTMapper)

	plugin := v1alpha1.NewPlugin(factory, client, restmapper, schemaresolver.New(apiextensionsFactory.Apiextensions().V1().CustomResourceDefinitions(), client.Discovery()), client, nil)
	if webhookServer == nil {
		if err := resetcluster(client); err != nil {
			panic(err)
		}

		if err := cel_polyfill.InstallCRDs(ctx, client); err != nil {
			panic(err)
		}

		certs, err := webhook.GenerateLocalCertificates()
		if err != nil {
			panic(err)
		}

		webhookServer = webhook.New(-1, certs, clientsetscheme.Scheme, webhookValidator)

		go func() {
			err := webhookServer.Run(context.Background())
			if err != nil {
				panic(err)
			}
		}()

		err = wait.Poll(250*time.Millisecond, 10*time.Second, func() (done bool, err error) {
			// When debugging, expect server to be running already. Treat
			// as non-fatal error if it isn't.
			// Install webhook configuration
			if err := webhookServer.Install(client); err != nil {
				fmt.Printf("failed to install webhook: %v\n", err.Error())
				return false, nil
			}

			// klog.Info("successfully updated webhook configuration")
			return true, nil
		})
		if err != nil {
			panic(err)
		}

	}

	webhookValidator.Set(plugin)
	go plugin.Run(ctx)

	factory.Start(ctx.Done())
	apiextensionsFactory.Start(ctx.Done())

	// wait for plugin to do initial sync
	err = wait.PollUntil(250*time.Millisecond, func() (done bool, err error) {
		return plugin.HasSynced(), nil
	}, ctx.Done())
	if err != nil {
		panic(err)
	}

	return NewTestClient(config), func() {
		webhookValidator.Set(noopValidator{})
		defer cancel()
		if err := resetcluster(client); err != nil {
			panic(err)
		}
	}
}

func resetclusterNamespace(client testClientInterface, namespace string) error {
	resources, err := client.Discovery().ServerPreferredNamespacedResources()
	if err != nil {
		return err
	}

	for _, rsrcList := range resources {
		gv, err := schema.ParseGroupVersion(rsrcList.GroupVersion)
		if err != nil {
			return err
		}
		for _, rsrc := range rsrcList.APIResources {
			if !rsrc.Namespaced || rsrc.Name == "serviceaccounts" {
				continue
			}
			gvr := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: rsrc.Name}
			resourceClient := client.Resource(gvr).Namespace(namespace)
			list, err := resourceClient.List(context.TODO(), metav1.ListOptions{})
			if err != nil && !errors.IsNotFound(err) && !errors.IsMethodNotSupported(err) {
				return err
			} else if err != nil || len(list.Items) == 0 {
				continue
			}
			fmt.Printf("deleting %v in namespace %v\n", gvr, namespace)

			for _, l := range list.Items {
				if strings.HasPrefix(l.GetName(), "kube") {
					continue
				}
				err = resourceClient.Delete(context.TODO(), l.GetName(), metav1.DeleteOptions{})
				if err != nil && !errors.IsNotFound(err) {
					return err
				}

				err = wait.PollImmediateWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
					_, err = resourceClient.Get(context.TODO(), l.GetName(), metav1.GetOptions{})
					if err != nil && errors.IsNotFound(err) {
						return true, nil
					}
					return false, err
				})
				if err != nil {
					return err
				}
			}
		}

	}

	// err = client.CoreV1().ConfigMaps(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
	// if err != nil {
	// 	return err
	// }

	// err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
	// 	l, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	// 	return (l != nil && len(l.Items) == 0), err
	// })
	// if err != nil {
	// 	return err
	// }

	// err = client.CoreV1().Endpoints(namespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
	// if err != nil {
	// 	return err
	// }

	// err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
	// 	l, err := client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{})
	// 	return (l != nil && len(l.Items) == 0), err
	// })
	// if err != nil {
	// 	return err
	// }

	return nil
}

func resetcluster(client testClientInterface) error {
	err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
		l, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().List(ctx, metav1.ListOptions{})
		return (l != nil && len(l.Items) == 0), err
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
		l, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().List(ctx, metav1.ListOptions{})
		return (l != nil && len(l.Items) == 0), err
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	namespaceList, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range namespaceList.Items {
		if strings.HasPrefix(n.Name, "kube") {
			continue
		}

		err = resetclusterNamespace(client, n.Name)
		if err != nil {
			return err
		}

		if n.Name == "default" {
			continue
		}

		err = client.CoreV1().Namespaces().Delete(context.TODO(), n.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}

		err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 10*time.Second, func(ctx context.Context) (done bool, err error) {
			_, err = client.CoreV1().Namespaces().Get(ctx, n.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})

		if err != nil {
			return err
		}
	}

	crdList, err := client.ApiextensionsV1().CustomResourceDefinitions().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, crd := range crdList.Items {
		if strings.Contains(crd.Name, "admissionregistration.polyfill.sigs.k8s.io") {
			continue
		}

		err = client.ApiextensionsV1().CustomResourceDefinitions().Delete(context.TODO(), crd.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}

		err = wait.PollWithContext(context.TODO(), 100*time.Millisecond, 2*time.Second, func(ctx context.Context) (done bool, err error) {
			_, err = client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			return err
		}

		err = wait.Poll(250*time.Millisecond, 10*time.Second, func() (done bool, err error) {
			if CrdExistsInDiscovery(client, &crd) {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func CreateTestCRDs(t *testing.T, client apiextensionsclientset.Interface, skipCrdExistsInDiscovery bool, crds ...*apiextensionsv1.CustomResourceDefinition) {
	for _, crd := range crds {
		createTestCRD(t, client, skipCrdExistsInDiscovery, crd)
	}
}

func createTestCRD(t *testing.T, client apiextensionsclientset.Interface, skipCrdExistsInDiscovery bool, crd *apiextensionsv1.CustomResourceDefinition) {
	if _, err := client.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create %s CRD; %v", crd.Name, err)
	}
	if skipCrdExistsInDiscovery {
		if err := waitForEstablishedCRD(client, crd.Name); err != nil {
			t.Fatalf("Failed to establish %s CRD; %v", crd.Name, err)
		}
		return
	}
	if err := wait.PollImmediate(500*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		return CrdExistsInDiscovery(client, crd), nil
	}); err != nil {
		t.Fatalf("Failed to see %s in discovery: %v", crd.Name, err)
	}
}

func waitForEstablishedCRD(client apiextensionsclientset.Interface, name string) error {
	return wait.PollImmediate(500*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		crd, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, cond := range crd.Status.Conditions {
			switch cond.Type {
			case apiextensionsv1.Established:
				if cond.Status == apiextensionsv1.ConditionTrue {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

// CrdExistsInDiscovery checks to see if the given CRD exists in discovery at all served versions.
func CrdExistsInDiscovery(client apiextensionsclientset.Interface, crd *apiextensionsv1.CustomResourceDefinition) bool {
	var versions []string
	for _, v := range crd.Spec.Versions {
		if v.Served {
			versions = append(versions, v.Name)
		}
	}
	for _, v := range versions {
		if !crdVersionExistsInDiscovery(client, crd, v) {
			return false
		}
	}
	return true
}

func crdVersionExistsInDiscovery(client apiextensionsclientset.Interface, crd *apiextensionsv1.CustomResourceDefinition, version string) bool {
	resourceList, err := client.Discovery().ServerResourcesForGroupVersion(crd.Spec.Group + "/" + version)
	if err != nil {
		return false
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == crd.Spec.Names.Plural {
			return true
		}
	}
	return false
}

// Test_ValidateNamespace_NoParams tests a ValidatingAdmissionPolicy that validates creation of a Namespace with no params.
func Test_ValidateNamespace_NoParams(t *testing.T) {
	forbiddenReason := metav1.StatusReasonForbidden

	testcases := []struct {
		name          string
		policy        *admissionregistrationv1alpha1.ValidatingAdmissionPolicy
		policyBinding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "namespace name contains suffix enforced by validating admission policy, using object metadata fields",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policyusing, object metadata fields",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-foobar",
				},
			},
			err:           "namespaces \"test-foobar\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policy using object metadata fields, AND validating expression returns StatusReasonForbidden",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
					Reason:     &forbiddenReason,
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "forbidden-test-foobar",
				},
			},
			err:           "namespaces \"forbidden-test-foobar\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonForbidden,
		},
		{
			name: "namespace name contains suffix enforced by validating admission policy, using request field",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "request.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "namespace name does NOT contains suffix enforced by validating admission policy, using request field",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "request.name.endsWith('k8s')",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "runtime error when validating namespace, but failurePolicy=Ignore",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.nonExistentProperty == 'someval'",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Ignore, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "runtime error when validating namespace, but failurePolicy=Fail",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.nonExistentProperty == 'someval'",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix")))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: expression 'object.nonExistentProperty == 'someval'' resulted in error: no such key: nonExistentProperty",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "runtime error due to unguarded params",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.startsWith(params.metadata.name)",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: expression 'object.metadata.name.startsWith(params.metadata.name)' resulted in error: no such key: metadata",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with check against unguarded params using has()",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "has(params.metadata) && has(params.metadata.name) && object.metadata.name.endsWith(params.metadata.name)",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: expression 'has(params.metadata) && has(params.metadata.name) && object.metadata.name.endsWith(params.metadata.name)' resulted in error: invalid type for field selection.",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with check against null params",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "(params != null && object.metadata.name.endsWith(params.metadata.name))",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err:           "namespaces \"test-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: (params != null && object.metadata.name.endsWith(params.metadata.name))",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with check against unguarded params using has() and default check",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "(has(params.metadata) && has(params.metadata.name) && object.metadata.name.startsWith(params.metadata.name)) || object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "with check against null params and default check",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "(params != null && object.metadata.name.startsWith(params.metadata.name)) || object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", ""),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			client, cleanup := serverScope(nil)
			t.Cleanup(cleanup)

			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := createAndWaitReady(t, client, testcase.policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			_, err := client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
		})
	}
}
func Test_ValidateAnnotationsAndWarnings(t *testing.T) {
	testcases := []struct {
		name             string
		policy           *admissionregistrationv1alpha1.ValidatingAdmissionPolicy
		policyBinding    *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding
		object           *v1.ConfigMap
		err              string
		failureReason    metav1.StatusReason
		auditAnnotations map[string]string
		warnings         sets.Set[string]
	}{
		{
			name: "with audit annotations",
			policy: withAuditAnnotations([]admissionregistrationv1alpha1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "'object name: ' + object.metadata.name",
				},
				{
					Key:             "exclude-key",
					ValueExpression: "null",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withConfigMapMatch(makePolicy("validate-audit-annotations"))))),
			policyBinding: makeBinding("validate-audit-annotations-binding", "validate-audit-annotations", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test1-k8s",
				},
			},
			err: "",
			auditAnnotations: map[string]string{
				"validate-audit-annotations/example-key": `object name: test1-k8s`,
			},
		},
		{
			name: "with audit annotations with invalid expression",
			policy: withAuditAnnotations([]admissionregistrationv1alpha1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "string(params.metadata.name)", // runtime error, params is null
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withConfigMapMatch(makePolicy("validate-audit-annotations-invalid"))))),
			policyBinding: makeBinding("validate-audit-annotations-invalid-binding", "validate-audit-annotations-invalid", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test2-k8s",
				},
			},
			err:           "configmaps \"test2-k8s\" is forbidden: ValidatingAdmissionPolicy 'validate-audit-annotations-invalid' with binding 'validate-audit-annotations-invalid-binding' denied request: expression 'string(params.metadata.name)' resulted in error: no such key: metadata",
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "with audit annotations with invalid expression and ignore failure policy",
			policy: withAuditAnnotations([]admissionregistrationv1alpha1.AuditAnnotation{
				{
					Key:             "example-key",
					ValueExpression: "string(params.metadata.name)", // runtime error, params is null
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Ignore, withConfigMapMatch(makePolicy("validate-audit-annotations-invalid-ignore"))))),
			policyBinding: makeBinding("validate-audit-annotations-invalid-ignore-binding", "validate-audit-annotations-invalid-ignore", ""),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test3-k8s",
				},
			},
			err: "",
		},
		{
			name: "with warn validationActions",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withConfigMapMatch(makePolicy("validate-actions-warn"))))),
			policyBinding: withValidationActions([]admissionregistrationv1alpha1.ValidationAction{admissionregistrationv1alpha1.Warn}, makeBinding("validate-actions-warn-binding", "validate-actions-warn", "")),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test4-nope",
				},
			},
			warnings: sets.New("Validation failed for ValidatingAdmissionPolicy 'validate-actions-warn' with binding 'validate-actions-warn-binding': failed expression: object.metadata.name.endsWith('k8s')"),
		},
		{
			name: "with audit validationActions",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith('k8s')",
				},
			}, withParams(configParamKind(), withFailurePolicy(admissionregistrationv1alpha1.Fail, withConfigMapMatch(makePolicy("validate-actions-audit"))))),
			policyBinding: withValidationActions([]admissionregistrationv1alpha1.ValidationAction{admissionregistrationv1alpha1.Deny, admissionregistrationv1alpha1.Audit}, makeBinding("validate-actions-audit-binding", "validate-actions-audit", "")),
			object: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test5-nope",
				},
			},
			err:           "configmaps \"test5-nope\" is forbidden: ValidatingAdmissionPolicy 'validate-actions-audit' with binding 'validate-actions-audit-binding' denied request: failed expression: object.metadata.name.endsWith('k8s')",
			failureReason: metav1.StatusReasonInvalid,
			auditAnnotations: map[string]string{
				"validation.policy.admission.k8s.io/validation_failure": `[{"message":"failed expression: object.metadata.name.endsWith('k8s')","policy":"validate-actions-audit","binding":"validate-actions-audit-binding","expressionIndex":1,"validationActions":["Deny","Audit"]}]`,
			},
		},
	}

	// prepare audit policy file
	policyFile, err := os.CreateTemp("", "audit-policy.yaml")
	if err != nil {
		t.Fatalf("Failed to create audit policy file: %v", err)
	}
	defer os.Remove(policyFile.Name())
	if _, err := policyFile.Write([]byte(auditPolicy)); err != nil {
		t.Fatalf("Failed to write audit policy file: %v", err)
	}
	if err := policyFile.Close(); err != nil {
		t.Fatalf("Failed to close audit policy file: %v", err)
	}

	// prepare audit log file
	logFile, err := os.CreateTemp("", "audit.log")
	if err != nil {
		t.Fatalf("Failed to create audit log file: %v", err)
	}
	defer os.Remove(logFile.Name())

	// SERVERSCOPE
	warnHandler := newWarningHandler()
	client, cleanup := serverScope(func(c *rest.Config) {
		c.WarningHandler = warnHandler
		c.Impersonate.UserName = testReinvocationClientUsername
	})
	t.Cleanup(cleanup)

	for i, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			testCaseID := strconv.Itoa(i)
			ns := "auditannotations-" + testCaseID
			_, err = client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}

			if err := createAndWaitReadyNamespacedWithWarnHandler(t, client, withMatchNamespace(testcase.policyBinding, ns), nil, ns, warnHandler); err != nil {
				t.Fatal(err)
			}
			warnHandler.reset()
			testcase.object.Namespace = ns
			_, err = client.CoreV1().ConfigMaps(ns).Create(context.TODO(), testcase.object, metav1.CreateOptions{})

			// code := int32(201)
			// if testcase.err != "" {
			// 	code = 422
			// }

			// auditAnnotationFilter := func(key, val string) bool {
			// 	_, ok := testcase.auditAnnotations[key]
			// 	return ok
			// }

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
			checkExpectedWarnings(t, warnHandler, testcase.warnings)
			// checkAuditEvents(t, logFile, expectedAuditEvents(testcase.auditAnnotations, ns, code), auditAnnotationFilter)
		})
	}
}

// Test_ValidateNamespace_WithConfigMapParams tests a ValidatingAdmissionPolicy that validates creation of a Namespace,
// using ConfigMap as a param reference.
func Test_ValidateNamespace_WithConfigMapParams(t *testing.T) {
	testcases := []struct {
		name          string
		policy        *admissionregistrationv1alpha1.ValidatingAdmissionPolicy
		policyBinding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding
		configMap     *v1.ConfigMap
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "namespace name contains suffix enforced by validating admission policy",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith(params.data.namespaceSuffix)",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withParams(configParamKind(), withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", "validate-namespace-suffix-param"),
			configMap: makeConfigParams("validate-namespace-suffix-param", map[string]string{
				"namespaceSuffix": "k8s",
			}),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-k8s",
				},
			},
			err: "",
		},
		{
			name: "namespace name does NOT contain suffix enforced by validating admission policy",
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.metadata.name.endsWith(params.data.namespaceSuffix)",
				},
			}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withParams(configParamKind(), withNamespaceMatch(makePolicy("validate-namespace-suffix"))))),
			policyBinding: makeBinding("validate-namespace-suffix-binding", "validate-namespace-suffix", "validate-namespace-suffix-param"),
			configMap: makeConfigParams("validate-namespace-suffix-param", map[string]string{
				"namespaceSuffix": "k8s",
			}),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-foo",
				},
			},
			err:           "namespaces \"test-foo\" is forbidden: ValidatingAdmissionPolicy 'validate-namespace-suffix' with binding 'validate-namespace-suffix-binding' denied request: failed expression: object.metadata.name.endsWith(params.data.namespaceSuffix)",
			failureReason: metav1.StatusReasonInvalid,
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			client, cleanup := serverScope(nil)
			t.Cleanup(cleanup)

			if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), testcase.configMap, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}

			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := createAndWaitReady(t, client, testcase.policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			_, err := client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})
			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
		})
	}
}

func TestMultiplePolicyBindings(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	paramKind := &admissionregistrationv1alpha1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}
	policy := withPolicyExistsLabels([]string{"paramIdent"}, withParams(paramKind, withPolicyMatch("secrets", withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("test-policy")))))
	policy.Spec.Validations = []admissionregistrationv1alpha1.Validation{
		{
			Expression: "params.data.autofail != 'true' && (params.data.conditional == 'false' || object.metadata.name.startsWith(params.data.check))",
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	autoFailParams := makeConfigParams("autofail-params", map[string]string{
		"autofail": "true",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), autoFailParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	autofailBinding := withBindingExistsLabels([]string{"autofail-binding-label"}, policy, makeBinding("autofail-binding", "test-policy", "autofail-params"))
	if err := createAndWaitReady(t, client, autofailBinding, map[string]string{"paramIdent": "true", "autofail-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	autoPassParams := makeConfigParams("autopass-params", map[string]string{
		"autofail":    "false",
		"conditional": "false",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), autoPassParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	autopassBinding := withBindingExistsLabels([]string{"autopass-binding-label"}, policy, makeBinding("autopass-binding", "test-policy", "autopass-params"))
	if err := createAndWaitReady(t, client, autopassBinding, map[string]string{"paramIdent": "true", "autopass-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	condpassParams := makeConfigParams("condpass-params", map[string]string{
		"autofail":    "false",
		"conditional": "true",
		"check":       "prefix-",
	})
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), condpassParams, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	condpassBinding := withBindingExistsLabels([]string{"condpass-binding-label"}, policy, makeBinding("condpass-binding", "test-policy", "condpass-params"))
	if err := createAndWaitReady(t, client, condpassBinding, map[string]string{"paramIdent": "true", "condpass-binding-label": "true"}); err != nil {
		t.Fatal(err)
	}

	autofailingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "autofailing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"autofail-binding-label": "true",
			},
		},
	}
	_, err := client.CoreV1().Secrets("default").Create(context.TODO(), autofailingSecret, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected secret creation to fail due to autofail-binding")
	}
	checkForFailedRule(t, err)
	checkFailureReason(t, err, metav1.StatusReasonInvalid)

	autopassingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "autopassing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"autopass-binding-label": "true",
			},
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), autopassingSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("expected secret creation to succeed, got: %s", err)
	}

	condpassingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prefix-condpassing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"condpass-binding-label": "true",
			},
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), condpassingSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("expected secret creation to succeed, got: %s", err)
	}

	condfailingSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "condfailing-secret",
			Labels: map[string]string{
				"paramIdent":             "someVal",
				"condpass-binding-label": "true",
			},
		},
	}
	_, err = client.CoreV1().Secrets("default").Create(context.TODO(), condfailingSecret, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("expected secret creation to fail due to autofail-binding")
	}
	checkForFailedRule(t, err)
	checkFailureReason(t, err, metav1.StatusReasonInvalid)
}

// Test_PolicyExemption tests that ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBinding resources
// are exempt from policy rules.
func Test_PolicyExemption(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := makePolicy("test-policy")
	policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{
		ResourceRules: []admissionregistrationv1alpha1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"*",
						},
						APIVersions: []string{
							"*",
						},
						Resources: []string{
							"*",
						},
					},
				},
			},
		},
	}

	policy.Spec.Validations = []admissionregistrationv1alpha1.Validation{{
		Expression: "false",
		Message:    "marker denied; policy is ready",
	}}

	policy, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("test-policy-binding", "test-policy", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	// validate that operations to ValidatingAdmissionPolicy are exempt from an existing policy that catches all resources
	policy, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Get(context.TODO(), policy.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ignoreFailurePolicy := admissionregistrationv1alpha1.Ignore
	policy.Spec.FailurePolicy = &ignoreFailurePolicy
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Update(context.TODO(), policy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	policyBinding, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), policyBinding.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that operations to ValidatingAdmissionPolicyBindings are exempt from an existing policy that catches all resources
	policyBindingCopy := policyBinding.DeepCopy()
	policyBindingCopy.Spec.PolicyName = "different-binding"
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Update(context.TODO(), policyBindingCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamKind validates the behavior of ValidatingAdmissionPolicy when
// only the ParamKind is updated. This test creates a policy where namespaces must have a prefix that matches
// the ParamKind set in the policy. Switching the ParamKind should result in only namespaces with prefixes matching
// the new ParamKind to be allowed. For example, when Paramkind is v1/ConfigMap, only namespaces prefixed with "configmap"
// is allowed and when ParamKind is updated to v1/Secret, only namespaces prefixed with "secret" is allowed, etc.
func Test_ValidatingAdmissionPolicy_UpdateParamKind(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	allowedPrefixesParamsConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allowed-prefixes",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allowed-prefixes",
		},
	}
	if _, err := client.CoreV1().Secrets("default").Create(context.TODO(), allowedPrefixesParamSecret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	paramKind := &admissionregistrationv1alpha1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.kind.lowerAscii())",
			Message:    "wrong paramKind",
		},
	}, withParams(paramKind, withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	policy, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "allowed-prefixes")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "configmap-" are allowed
	// and namespaces starting with "secret-" are disallowed
	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "configmap-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	disallowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
	if err == nil {
		t.Error("unexpected nil error")
	}
	if !strings.Contains(err.Error(), "wrong paramKind") {
		t.Errorf("unexpected error message: %v", err)
	}
	checkFailureReason(t, err, metav1.StatusReasonInvalid)

	// update the policy ParamKind to reference a Secret
	paramKind = &admissionregistrationv1alpha1.ParamKind{
		APIVersion: "v1",
		Kind:       "Secret",
	}
	policy, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Get(context.TODO(), policy.Name, metav1.GetOptions{})
	if err != nil {
		t.Error(err)
	}
	policy.Spec.ParamKind = paramKind
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Update(context.TODO(), policy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	// validate that namespaces starting with "secret-" are allowed
	// and namespaces starting with "configmap-" are disallowed
	// wait loop is required here since ConfigMaps were previousy allowed and we need to wait for the new policy
	// to be enforced
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace = &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "configmap-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramKind") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "secret-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamRef validates the behavior of ValidatingAdmissionPolicy when
// only the ParamRef in the binding is updated. This test creates a policy where namespaces must have a prefix that matches
// the ParamRef set in the policy binding. The paramRef in the binding is then updated to a different object.
func Test_ValidatingAdmissionPolicy_UpdateParamRef(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	allowedPrefixesParamsConfigMap1 := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap1, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	allowedPrefixesParamsConfigMap2 := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), allowedPrefixesParamsConfigMap2, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.metadata.name)",
			Message:    "wrong paramRef",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-1" are allowed
	// and namespaces starting with "test-2-" are disallowed
	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "test-1")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-2-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramRef") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-1-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	// Update the paramRef in the policy binding to use the test-2 ConfigMap
	policyBinding, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), allowedPrefixesBinding.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBindingCopy := policyBinding.DeepCopy()
	policyBindingCopy.Spec.ParamRef = &admissionregistrationv1alpha1.ParamRef{
		Name:      "test-2",
		Namespace: "default",
	}
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Update(context.TODO(), policyBindingCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Error(err)
	}

	// validate that namespaces starting with "test-2" are allowed
	// and namespaces starting with "test-1" are disallowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-1-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong paramRef") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-2-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_UpdateParamResource validates behavior of a policy after updates to the param resource.
func Test_ValidatingAdmissionPolicy_UpdateParamResource(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	paramConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed-prefix",
			Namespace: "default",
		},
		Data: map[string]string{
			"prefix": "test-1",
		},
	}
	paramConfigMap, err := client.CoreV1().ConfigMaps(paramConfigMap.Namespace).Create(context.TODO(), paramConfigMap, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.data['prefix'])",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-1" are allowed
	// and namespaces starting with "test-2-" are disallowed
	allowedPrefixesBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "allowed-prefix")
	if err := createAndWaitReady(t, client, allowedPrefixesBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-2-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-1-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}

	// Update the param resource to use "test-2" as the new allwoed prefix
	paramConfigMapCopy := paramConfigMap.DeepCopy()
	paramConfigMapCopy.Data = map[string]string{
		"prefix": "test-2",
	}
	_, err = client.CoreV1().ConfigMaps(paramConfigMapCopy.Namespace).Update(context.TODO(), paramConfigMapCopy, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test-2" are allowed
	// and namespaces starting with "test-1" are disallowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-1-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	allowedNamespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-2-",
		},
	}
	_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByObjectSelector(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"foo": "bar",
		},
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "matched by object selector!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-object-selector"))))
	policy = withObjectSelector(labelSelector, policy)
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-object-selector-binding", "match-by-object-selector", "")
	if err := createAndWaitReady(t, client, policyBinding, map[string]string{"foo": "bar"}); err != nil {
		t.Fatal(err)
	}

	matchedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "denied",
			Namespace: "default",
			Labels: map[string]string{
				"foo": "bar",
			},
		},
	}

	_, err = client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by object selector!") {
		t.Errorf("unexpected error: %v", err)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByNamespaceSelector(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	// only configmaps in default will be allowed.
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "kubernetes.io/metadata.name",
				Operator: "NotIn",
				Values:   []string{"default"},
			},
		},
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "matched by namespace selector!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-namespace-selector"))))
	policy = withNamespaceSelector(labelSelector, policy)
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-namespace-selector-binding", "match-by-namespace-selector", "")
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-default",
		},
	}
	if _, err := client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		matchedConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "denied-",
				Namespace:    "not-default",
			},
		}

		_, err := client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
		// policy not enforced yet, try again
		if err == nil {
			return false, nil
		}

		if !strings.Contains(err.Error(), "matched by namespace selector!") {
			return false, err
		}

		return true, nil

	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allowed",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchByResourceNames(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "matched by resource names!",
		},
	}, withConfigMapMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-resource-names"))))
	policy.Spec.MatchConstraints.ResourceRules[0].ResourceNames = []string{"matched-by-resource-name"}
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-resource-names-binding", "match-by-resource-names", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	matchedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matched-by-resource-name",
			Namespace: "default",
		},
	}

	_, err = client.CoreV1().ConfigMaps(matchedConfigMap.Namespace).Create(context.TODO(), matchedConfigMap, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by resource names!") {
		t.Errorf("unexpected error: %v", err)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-matched-by-resource-name",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithExcludeResources(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "not matched by exclude resources!",
		},
	}, withPolicyMatch("*", withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-resource-names"))))

	policy = withExcludePolicyMatch("configmaps", policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-resource-names-binding", "match-by-resource-names", "")
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		secret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-matched-by-exclude-resources",
				Namespace:    "default",
			},
		}

		_, err := client.CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		// policy not enforced yet, try again
		if err == nil {
			return false, nil
		}

		if !strings.Contains(err.Error(), "not matched by exclude resources!") {
			return false, err
		}

		return true, nil

	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	allowedConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matched-by-exclude-resources",
			Namespace: "default",
		},
	}

	if _, err := client.CoreV1().ConfigMaps(allowedConfigMap.Namespace).Create(context.TODO(), allowedConfigMap, metav1.CreateOptions{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithMatchPolicyEquivalent(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	CreateTestCRDs(t, client, false, versionedCustomResourceDefinition())

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "matched by equivalent match policy!",
		},
	}, withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-match-policy-equivalent")))
	policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{
		ResourceRules: []admissionregistrationv1alpha1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"awesome.bears.com",
						},
						APIVersions: []string{
							"v1",
						},
						Resources: []string{
							"pandas",
						},
					},
				},
			},
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-match-policy-equivalent-binding", "match-by-match-policy-equivalent", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	v1Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v1",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v1-bears",
			},
		},
	}

	v2Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v2",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v2-bears",
			},
		},
	}

	_, err := client.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}).Create(context.TODO(), v1Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by equivalent match policy!") {
		t.Errorf("v1 panadas did not match against policy, err: %v", err)
	}

	_, err = client.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v2", Resource: "pandas"}).Create(context.TODO(), v2Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by equivalent match policy!") {
		t.Errorf("v2 panadas did not match against policy, err: %v", err)
	}
}

func Test_ValidatingAdmissionPolicy_MatchWithMatchPolicyExact(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	CreateTestCRDs(t, client, false, versionedCustomResourceDefinition())

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "matched by exact match policy!",
		},
	}, withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("match-by-match-policy-exact")))
	matchPolicyExact := admissionregistrationv1alpha1.Exact
	policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{
		MatchPolicy: &matchPolicyExact,
		ResourceRules: []admissionregistrationv1alpha1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"awesome.bears.com",
						},
						APIVersions: []string{
							"v1",
						},
						Resources: []string{
							"pandas",
						},
					},
				},
			},
		},
	}
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policyBinding := makeBinding("match-by-match-policy-exact-binding", "match-by-match-policy-exact", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	v1Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v1",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v1-bears",
			},
		},
	}

	v2Resource := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "awesome.bears.com" + "/" + "v2",
			"kind":       "Panda",
			"metadata": map[string]interface{}{
				"name": "v2-bears",
			},
		},
	}

	_, err := client.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}).Create(context.TODO(), v1Resource, metav1.CreateOptions{})
	if !strings.Contains(err.Error(), "matched by exact match policy!") {
		t.Errorf("v1 panadas did not match against policy, err: %v", err)
	}

	// v2 panadas is allowed since policy specificed match policy Exact and only matched against v1
	_, err = client.Resource(schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v2", Resource: "pandas"}).Create(context.TODO(), v2Resource, metav1.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
}

// Test_ValidatingAdmissionPolicy_PolicyDeletedThenRecreated validates that deleting a ValidatingAdmissionPolicy
// removes the policy from the apiserver admission chain and recreating it re-enables it.
func Test_ValidatingAdmissionPolicy_PolicyDeletedThenRecreated(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith('test')",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete the binding object and validate that policy is not enforced
	if err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Delete(context.TODO(), "allowed-prefixes", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return true, nil
		}

		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		return false, err
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// Test_ValidatingAdmissionPolicy_BindingDeletedThenRecreated validates that deleting a ValidatingAdmissionPolicyBinding
// removes the policy from the apiserver admission chain and recreating it re-enables it.
func Test_ValidatingAdmissionPolicy_BindingDeletedThenRecreated(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith('test')",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete the binding object and validate that policy is not enforced
	if err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Delete(context.TODO(), "allowed-prefixes-binding", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return true, nil
		}

		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		return false, err
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// recreate the policy binding and test that policy is enforced again
	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), policyBinding, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// Test_ValidatingAdmissionPolicy_ParamResourceDeletedThenRecreated validates that deleting a param resource referenced
// by a binding renders the policy as invalid. Recreating the param resource re-enables the policy.
func Test_ValidatingAdmissionPolicy_ParamResourceDeletedThenRecreated(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	param := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), param, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "object.metadata.name.startsWith(params.metadata.name)",
			Message:    "wrong prefix",
		},
	}, withParams(configParamKind(), withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("allowed-prefixes")))))
	policy = withWaitReadyConstraintAndExpression(policy)
	_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// validate that namespaces starting with "test" are allowed
	policyBinding := makeBinding("allowed-prefixes-binding", "allowed-prefixes", "test")
	if err := createAndWaitReady(t, client, policyBinding, nil); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		if err == nil {
			return false, nil
		}

		if strings.Contains(err.Error(), "not yet synced to use for admission") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// delete param object and validate that policy is invalid
	if err := client.CoreV1().ConfigMaps("default").Delete(context.TODO(), "test", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		allowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), allowedNamespace, metav1.CreateOptions{})
		// old policy is still enforced, try again
		if strings.Contains(err.Error(), "wrong prefix") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "failed to configure binding: test not found") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}

	// recreate the param resource and validate namespace is disallowed again
	if _, err := client.CoreV1().ConfigMaps("default").Create(context.TODO(), param, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		disallowedNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "not-test-",
			},
		}

		_, err = client.CoreV1().Namespaces().Create(context.TODO(), disallowedNamespace, metav1.CreateOptions{})
		// cache not synced with new object yet, try again
		if strings.Contains(err.Error(), "failed to configure binding: test not found") {
			return false, nil
		}

		if !strings.Contains(err.Error(), "wrong prefix") {
			return false, err
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", err)
	}
}

// TestCRDParams tests that a CustomResource can be used as a param resource for a ValidatingAdmissionPolicy.
func TestCRDParams(t *testing.T) {
	testcases := []struct {
		name          string
		resource      *unstructured.Unstructured
		policy        *admissionregistrationv1alpha1.ValidatingAdmissionPolicy
		policyBinding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding
		namespace     *v1.Namespace
		err           string
		failureReason metav1.StatusReason
	}{
		{
			name: "a rule that uses data from a CRD param resource does NOT pass",
			resource: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "awesome.bears.com/v1",
				"kind":       "Panda",
				"metadata": map[string]interface{}{
					"name": "config-obj",
				},
				"spec": map[string]interface{}{
					"nameCheck": "crd-test-k8s",
				},
			}},
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "params.spec.nameCheck == object.metadata.name",
				},
			}, withNamespaceMatch(withParams(withCRDParamKind("Panda", "awesome.bears.com", "v1"), withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("test-policy"))))),
			policyBinding: makeBinding("crd-policy-binding", "test-policy", "config-obj"),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "incorrect-name",
				},
			},
			err:           `namespaces "incorrect-name" is forbidden: ValidatingAdmissionPolicy 'test-policy' with binding 'crd-policy-binding' denied request: failed expression: params.spec.nameCheck == object.metadata.name`,
			failureReason: metav1.StatusReasonInvalid,
		},
		{
			name: "a rule that uses data from a CRD param resource that does pass",
			resource: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "awesome.bears.com/v1",
				"kind":       "Panda",
				"metadata": map[string]interface{}{
					"name": "config-obj",
				},
				"spec": map[string]interface{}{
					"nameCheck": "crd-test-k8s",
				},
			}},
			policy: withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "params.spec.nameCheck == object.metadata.name",
				},
			}, withNamespaceMatch(withParams(withCRDParamKind("Panda", "awesome.bears.com", "v1"), withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("test-policy"))))),
			policyBinding: makeBinding("crd-policy-binding", "test-policy", "config-obj"),
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "crd-test-k8s",
				},
			},
			err: ``,
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			client, cleanup := serverScope(nil)
			t.Cleanup(cleanup)

			crd := versionedCustomResourceDefinition()
			CreateTestCRDs(t, client, false, crd)

			gvr := schema.GroupVersionResource{
				Group:    crd.Spec.Group,
				Version:  crd.Spec.Versions[0].Name,
				Resource: crd.Spec.Names.Plural,
			}
			crClient := client.Resource(gvr)
			_, err := crClient.Create(context.TODO(), testcase.resource, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("error creating %s: %s", gvr, err)
			}

			policy := withWaitReadyConstraintAndExpression(testcase.policy)
			if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			// remove default namespace since the CRD is cluster-scoped
			testcase.policyBinding.Spec.ParamRef.Namespace = ""
			if err := createAndWaitReady(t, client, testcase.policyBinding, nil); err != nil {
				t.Fatal(err)
			}

			_, err = client.CoreV1().Namespaces().Create(context.TODO(), testcase.namespace, metav1.CreateOptions{})

			checkExpectedError(t, err, testcase.err)
			checkFailureReason(t, err, testcase.failureReason)
		})
	}
}

func TestBindingRemoval(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	policy := withValidations([]admissionregistrationv1alpha1.Validation{
		{
			Expression: "false",
			Message:    "policy still in effect",
		},
	}, withNamespaceMatch(withFailurePolicy(admissionregistrationv1alpha1.Fail, makePolicy("test-policy"))))
	policy = withWaitReadyConstraintAndExpression(policy)
	if _, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	binding := makeBinding("test-binding", "test-policy", "test-params")
	if err := createAndWaitReady(t, client, binding, nil); err != nil {
		t.Fatal(err)
	}
	// check that the policy is active
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		namespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "check-namespace",
			},
		}
		_, err := client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "policy still in effect") {
				return true, nil
			} else {
				// unexpected error while attempting namespace creation
				return true, err
			}
		}
		return false, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}
	if err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Delete(context.TODO(), "test-binding", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	// wait for binding to be deleted
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {

		_, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Get(context.TODO(), "test-binding", metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			} else {
				return true, err
			}
		}

		return false, nil
	}); waitErr != nil {
		t.Errorf("timed out waiting: %v", waitErr)
	}

	// policy should be considered in an invalid state and namespace creation should be allowed
	if waitErr := wait.PollImmediate(time.Millisecond*10, wait.ForeverTestTimeout, func() (bool, error) {
		namespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-namespace",
			},
		}
		_, err := client.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
		if err != nil {
			t.Logf("namespace creation failed: %s", err)
			return false, nil
		}

		return true, nil
	}); waitErr != nil {
		t.Errorf("expected namespace creation to succeed but timed out waiting: %v", waitErr)
	}
}

// Test_ValidateSecondaryAuthorization tests a ValidatingAdmissionPolicy that performs secondary authorization checks
// for both users and service accounts.
// func Test_ValidateSecondaryAuthorization(t *testing.T) {
// 	testcases := []struct {
// 		name             string
// 		rbac             *rbacv1.PolicyRule
// 		expression       string
// 		allowed          bool
// 		extraAccountFn   func(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface
// 		extraAccountRbac *rbacv1.PolicyRule
// 	}{
// 		{
// 			name: "principal is allowed to create a specific deployment",
// 			rbac: &rbacv1.PolicyRule{
// 				Verbs:         []string{"create"},
// 				APIGroups:     []string{"apps"},
// 				Resources:     []string{"deployments/status"},
// 				ResourceNames: []string{"charmander"},
// 			},
// 			expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('default').namespace('default').name('charmander').check('create').allowed()",
// 			allowed:    true,
// 		},
// 		{
// 			name:       "principal is not allowed to create a specific deployment",
// 			expression: "authorizer.group('apps').resource('deployments').subresource('status').namespace('default').name('charmander').check('create').allowed()",
// 			allowed:    false,
// 		},
// 		{
// 			name: "principal is authorized for custom verb on current resource",
// 			rbac: &rbacv1.PolicyRule{
// 				Verbs:     []string{"anthropomorphize"},
// 				APIGroups: []string{""},
// 				Resources: []string{"namespaces"},
// 			},
// 			expression: "authorizer.requestResource.check('anthropomorphize').allowed()",
// 			allowed:    true,
// 		},
// 		{
// 			name:       "principal is not authorized for custom verb on current resource",
// 			expression: "authorizer.requestResource.check('anthropomorphize').allowed()",
// 			allowed:    false,
// 		},
// 		{
// 			name:           "serviceaccount is authorized for custom verb on current resource",
// 			extraAccountFn: serviceAccountClient("default", "extra-acct"),
// 			extraAccountRbac: &rbacv1.PolicyRule{
// 				Verbs:     []string{"anthropomorphize"},
// 				APIGroups: []string{""},
// 				Resources: []string{"pods"},
// 			},
// 			expression: "authorizer.serviceAccount('default', 'extra-acct').group('').resource('pods').check('anthropomorphize').allowed()",
// 			allowed:    true,
// 		},
// 	}

// 	for _, testcase := range testcases {
// 		t.Run(testcase.name, func(t *testing.T) {
// 			clients := map[string]func(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface{
// 				"user":           secondaryAuthorizationUserClient,
// 				"serviceaccount": secondaryAuthorizationServiceAccountClient,
// 			}

// 			for clientName, clientFn := range clients {
// 				t.Run(clientName, func(t *testing.T) {
// 					// SERVERSCOPE

// 					// For test set up such as creating policies, bindings and RBAC rules.
// 					adminClient := clientset.NewForConfigOrDie(server.ClientConfig)

// 					// Principal is always allowed to create and update namespaces so that the admission requests to test
// 					// authorization expressions can be sent by the principal.
// 					rules := []rbacv1.PolicyRule{{
// 						Verbs:     []string{"create", "update"},
// 						APIGroups: []string{""},
// 						Resources: []string{"namespaces"},
// 					}}
// 					if testcase.rbac != nil {
// 						rules = append(rules, *testcase.rbac)
// 					}

// 					client := clientFn(t, adminClient, server.ClientConfig, rules)

// 					if testcase.extraAccountFn != nil {
// 						var extraRules []rbacv1.PolicyRule
// 						if testcase.extraAccountRbac != nil {
// 							extraRules = append(rules, *testcase.extraAccountRbac)
// 						}
// 						testcase.extraAccountFn(t, adminClient, server.ClientConfig, extraRules)
// 					}

// 					policy := withWaitReadyConstraintAndExpression(withValidations([]admissionregistrationv1alpha1.Validation{
// 						{
// 							Expression: testcase.expression,
// 						},
// 					}, withFailurePolicy(admissionregistrationv1alpha1.Fail, withNamespaceMatch(makePolicy("validate-authz")))))
// 					if _, err := adminClient.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(context.TODO(), policy, metav1.CreateOptions{}); err != nil {
// 						t.Fatal(err)
// 					}
// 					if err := createAndWaitReady(t, adminClient, makeBinding("validate-authz-binding", "validate-authz", ""), nil); err != nil {
// 						t.Fatal(err)
// 					}

// 					ns := &v1.Namespace{
// 						ObjectMeta: metav1.ObjectMeta{
// 							Name: "test-authz",
// 						},
// 					}
// 					_, err = client.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})

// 					var expected metav1.StatusReason = ""
// 					if !testcase.allowed {
// 						expected = metav1.StatusReasonInvalid
// 					}
// 					checkFailureReason(t, err, expected)
// 				})
// 			}
// 		})
// 	}
// }

type clientFn func(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface

// func secondaryAuthorizationUserClient(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface {
// 	clientConfig = rest.CopyConfig(clientConfig)
// 	clientConfig.Impersonate = rest.ImpersonationConfig{
// 		UserName: "alice",
// 		UID:      "1234",
// 	}
// 	client := clientset.NewForConfigOrDie(clientConfig)

// 	for _, rule := range rules {
// 		authutil.GrantUserAuthorization(t, context.TODO(), adminClient, "alice", rule)
// 	}
// 	return client
// }

// func secondaryAuthorizationServiceAccountClient(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface {
// 	return serviceAccountClient("default", "test-service-acct")(t, adminClient, clientConfig, rules)
// }

// func serviceAccountClient(namespace, name string) clientFn {
// 	return func(t *testing.T, adminClient kubernetes.Interface, clientConfig *rest.Config, rules []rbacv1.PolicyRule) kubernetes.Interface {
// 		clientConfig = rest.CopyConfig(clientConfig)
// 		sa, err := adminClient.CoreV1().ServiceAccounts(namespace).Create(context.TODO(), &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
// 		if err != nil {
// 			t.Fatal(err)
// 		}
// 		uid := sa.UID

// 		clientConfig.Impersonate = rest.ImpersonationConfig{
// 			UserName: "system:serviceaccount:" + namespace + ":" + name,
// 			UID:      string(uid),
// 		}
// 		client := clientset.NewForConfigOrDie(clientConfig)

// 		for _, rule := range rules {
// 			authutil.GrantServiceAccountAuthorization(t, context.TODO(), adminClient, name, namespace, rule)
// 		}
// 		return client
// 	}
// }

func withWaitReadyConstraintAndExpression(policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy = policy.DeepCopy()
	policy.Spec.MatchConstraints.ResourceRules = append(policy.Spec.MatchConstraints.ResourceRules, admissionregistrationv1alpha1.NamedRuleWithOperations{
		ResourceNames: []string{"test-marker"},
		RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{
				"UPDATE",
			},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{
					"",
				},
				APIVersions: []string{
					"v1",
				},
				Resources: []string{
					"endpoints",
				},
			},
		},
	})
	policy.Spec.Validations = append([]admissionregistrationv1alpha1.Validation{{
		Expression: "object.metadata.name != 'test-marker'",
		Message:    "marker denied; policy is ready",
	}}, policy.Spec.Validations...)
	return policy
}

func createAndWaitReady(t *testing.T, client kubernetes.Interface, binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string) error {
	return createAndWaitReadyNamespaced(t, client, binding, matchLabels, "default")
}

func createAndWaitReadyNamespaced(t *testing.T, client kubernetes.Interface, binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string, ns string) error {
	return createAndWaitReadyNamespacedWithWarnHandler(t, client, binding, matchLabels, ns, newWarningHandler())
}

func createAndWaitReadyNamespacedWithWarnHandler(t *testing.T, client kubernetes.Interface, binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding, matchLabels map[string]string, ns string, handler *warningHandler) error {
	marker := &v1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "test-marker", Namespace: ns, Labels: matchLabels}}
	defer func() {
		err := client.CoreV1().Endpoints(ns).Delete(context.TODO(), marker.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("error deleting marker: %v", err)
		}
	}()
	marker, err := client.CoreV1().Endpoints(ns).Create(context.TODO(), marker, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	_, err = client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicyBindings().Create(context.TODO(), binding, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	if waitErr := wait.PollImmediate(time.Millisecond*5, wait.ForeverTestTimeout, func() (bool, error) {
		handler.reset()
		_, err := client.CoreV1().Endpoints(ns).Patch(context.TODO(), marker.Name, types.JSONPatchType, []byte("[]"), metav1.PatchOptions{})
		if handler.hasObservedMarker() {
			return true, nil
		}
		if err != nil && strings.Contains(err.Error(), "marker denied; policy is ready") {
			return true, nil
		} else if err != nil && strings.Contains(err.Error(), "not yet synced to use for admission") {
			t.Logf("waiting for policy to be ready. Marker: %v. Admission not synced yet: %v", marker, err)
			return false, nil
		} else {
			t.Logf("waiting for policy to be ready. Marker: %v, Last marker patch response: %v", marker, err)
			return false, err
		}
	}); waitErr != nil {
		return waitErr
	}
	t.Logf("Marker ready: %v", marker)
	handler.reset()
	return nil
}

func withMatchNamespace(binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding, ns string) *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding {
	binding.Spec.MatchResources = &admissionregistrationv1alpha1.MatchResources{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{ns},
				},
			},
		},
	}
	return binding
}

func makePolicy(name string) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	return &admissionregistrationv1alpha1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func withParams(params *admissionregistrationv1alpha1.ParamKind, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.ParamKind = params
	return policy
}

func configParamKind() *admissionregistrationv1alpha1.ParamKind {
	return &admissionregistrationv1alpha1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}
}

func withFailurePolicy(failure admissionregistrationv1alpha1.FailurePolicyType, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.FailurePolicy = &failure
	return policy
}

func withNamespaceMatch(policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	return withPolicyMatch("namespaces", policy)
}

func withConfigMapMatch(policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	return withPolicyMatch("configmaps", policy)
}

func withObjectSelector(labelSelector *metav1.LabelSelector, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.ObjectSelector = labelSelector
	return policy
}

func withNamespaceSelector(labelSelector *metav1.LabelSelector, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.NamespaceSelector = labelSelector
	return policy
}

func withPolicyMatch(resource string, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{
		ResourceRules: []admissionregistrationv1alpha1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{
							"",
						},
						APIVersions: []string{
							"*",
						},
						Resources: []string{
							resource,
						},
					},
				},
			},
		},
	}
	return policy
}

func withExcludePolicyMatch(resource string, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints.ExcludeResourceRules = []admissionregistrationv1alpha1.NamedRuleWithOperations{
		{
			RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{
					"*",
				},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{
						"",
					},
					APIVersions: []string{
						"*",
					},
					Resources: []string{
						resource,
					},
				},
			},
		},
	}
	return policy
}

func withPolicyExistsLabels(labels []string, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	if policy.Spec.MatchConstraints == nil {
		policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{}
	}
	matchExprs := buildExistsSelector(labels)
	policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{
		MatchExpressions: matchExprs,
	}
	return policy
}

func withGVRMatch(groups []string, versions []string, resources []string, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.MatchConstraints = &admissionregistrationv1alpha1.MatchResources{
		ResourceRules: []admissionregistrationv1alpha1.NamedRuleWithOperations{
			{
				RuleWithOperations: admissionregistrationv1alpha1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{
						"*",
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   groups,
						APIVersions: versions,
						Resources:   resources,
					},
				},
			},
		},
	}
	return policy
}

func withValidations(validations []admissionregistrationv1alpha1.Validation, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.Validations = validations
	return policy
}

func withAuditAnnotations(auditAnnotations []admissionregistrationv1alpha1.AuditAnnotation, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy) *admissionregistrationv1alpha1.ValidatingAdmissionPolicy {
	policy.Spec.AuditAnnotations = auditAnnotations
	return policy
}

func makeBinding(name, policyName, paramName string) *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding {
	var paramRef *admissionregistrationv1alpha1.ParamRef
	if paramName != "" {
		paramRef = &admissionregistrationv1alpha1.ParamRef{
			Name:      paramName,
			Namespace: "default",
		}
	}
	return &admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1alpha1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        policyName,
			ParamRef:          paramRef,
			ValidationActions: []admissionregistrationv1alpha1.ValidationAction{admissionregistrationv1alpha1.Deny},
		},
	}
}

func withValidationActions(validationActions []admissionregistrationv1alpha1.ValidationAction, binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding {
	binding.Spec.ValidationActions = validationActions
	return binding
}

func withBindingExistsLabels(labels []string, policy *admissionregistrationv1alpha1.ValidatingAdmissionPolicy, binding *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1alpha1.ValidatingAdmissionPolicyBinding {
	if policy != nil {
		// shallow copy
		constraintsCopy := *policy.Spec.MatchConstraints
		binding.Spec.MatchResources = &constraintsCopy
	}
	matchExprs := buildExistsSelector(labels)
	binding.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{
		MatchExpressions: matchExprs,
	}
	return binding
}

func buildExistsSelector(labels []string) []metav1.LabelSelectorRequirement {
	matchExprs := make([]metav1.LabelSelectorRequirement, len(labels))
	for i := 0; i < len(labels); i++ {
		matchExprs[i].Key = labels[i]
		matchExprs[i].Operator = metav1.LabelSelectorOpExists
	}
	return matchExprs
}

func makeConfigParams(name string, data map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Data:       data,
	}
}

func checkForFailedRule(t *testing.T, err error) {
	if !strings.Contains(err.Error(), "failed expression") {
		t.Fatalf("unexpected error (expected to find \"failed expression\"): %s", err)
	}
	if strings.Contains(err.Error(), "evaluation error") {
		t.Fatalf("CEL rule evaluation failed: %s", err)
	}
}

func checkFailureReason(t *testing.T, err error, expectedReason metav1.StatusReason) {
	if err == nil && expectedReason == "" {
		// no reason was given, no error was passed - early exit
		return
	}
	switch e := err.(type) {
	case apierrors.APIStatus:
		reason := e.Status().Reason
		if reason != expectedReason {
			t.Logf("actual error reason: %v", reason)
			t.Logf("expected failure reason: %v", expectedReason)
			t.Error("Unexpected error reason")
		}
	default:
		t.Errorf("Unexpected error: %v", err)
	}
}

func checkExpectedWarnings(t *testing.T, recordedWarnings *warningHandler, expectedWarnings sets.Set[string]) {
	if !recordedWarnings.equals(expectedWarnings) {
		t.Errorf("Expected warnings '%v' but got '%v", expectedWarnings, recordedWarnings)
	}
}

// func checkAuditEvents(t *testing.T, logFile *os.File, auditEvents []utils.AuditEvent, filter utils.AuditAnnotationsFilter) {
// 	stream, err := os.OpenFile(logFile.Name(), os.O_RDWR, 0600)
// 	if err != nil {
// 		t.Errorf("unexpected error: %v", err)
// 	}
// 	defer stream.Close()

// 	if auditEvents != nil {
// 		missing, err := utils.CheckAuditLinesFiltered(stream, auditEvents, auditv1.SchemeGroupVersion, filter)
// 		if err != nil {
// 			t.Errorf("unexpected error checking audit lines: %v", err)
// 		}
// 		if len(missing.MissingEvents) > 0 {
// 			t.Errorf("failed to get expected events -- missing: %s", missing)
// 		}
// 	}
// 	if err := stream.Truncate(0); err != nil {
// 		t.Errorf("unexpected error truncate file: %v", err)
// 	}
// 	if _, err := stream.Seek(0, 0); err != nil {
// 		t.Errorf("unexpected error reset offset: %v", err)
// 	}
// }

func withCRDParamKind(kind, crdGroup, crdVersion string) *admissionregistrationv1alpha1.ParamKind {
	return &admissionregistrationv1alpha1.ParamKind{
		APIVersion: crdGroup + "/" + crdVersion,
		Kind:       kind,
	}
}

func checkExpectedError(t *testing.T, err error, expectedErr string) {
	if err == nil && expectedErr == "" {
		return
	}
	if err == nil && expectedErr != "" {
		t.Logf("actual error: %v", err)
		t.Logf("expected error: %v", expectedErr)
		t.Fatal("got nil error but expected an error")
	}

	if err != nil && expectedErr == "" {
		t.Logf("actual error: %v", err)
		t.Logf("expected error: %v", expectedErr)
		t.Fatal("got error but expected none")
	}

	prefix := "admission webhook \"cel-admission-polyfill.k8s.io\" denied the request: "
	timmed := strings.TrimPrefix(err.Error(), prefix)

	if timmed != expectedErr {
		t.Logf("actual validation error: %v", err)
		t.Logf("expected validation error: %v", expectedErr)
		t.Error("unexpected validation error")
	}
}

// Copied from etcd.GetCustomResourceDefinitionData
func versionedCustomResourceDefinition() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pandas.awesome.bears.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "awesome.bears.com",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
							LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
						},
					},
				},
				{
					Name:    "v2",
					Served:  true,
					Storage: false,
					Schema:  fixtures.AllowAllSchema(),
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
						Scale: &apiextensionsv1.CustomResourceSubresourceScale{
							SpecReplicasPath:   ".spec.replicas",
							StatusReplicasPath: ".status.replicas",
							LabelSelectorPath:  func() *string { path := ".status.selector"; return &path }(),
						},
					},
				},
			},
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "pandas",
				Kind:   "Panda",
			},
		},
	}
}

type warningHandler struct {
	lock           sync.Mutex
	warnings       sets.Set[string]
	observedMarker bool
}

func newWarningHandler() *warningHandler {
	return &warningHandler{warnings: sets.New[string]()}
}

func (w *warningHandler) reset() {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings = sets.New[string]()
	w.observedMarker = false
}

func (w *warningHandler) equals(s sets.Set[string]) bool {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.warnings.Equal(s)
}

func (w *warningHandler) hasObservedMarker() bool {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.observedMarker
}

func (w *warningHandler) HandleWarningHeader(code int, _ string, message string) {
	if strings.HasSuffix(message, "marker denied; policy is ready") {
		func() {
			w.lock.Lock()
			defer w.lock.Unlock()
			w.observedMarker = true
		}()
	}
	if code != 299 || len(message) == 0 {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	w.warnings.Insert(message)
}

// func expectedAuditEvents(auditAnnotations map[string]string, ns string, code int32) []utils.AuditEvent {
// 	return []utils.AuditEvent{
// 		{
// 			Level:                  auditinternal.LevelRequest,
// 			Stage:                  auditinternal.StageResponseComplete,
// 			RequestURI:             fmt.Sprintf("/api/v1/namespaces/%s/configmaps", ns),
// 			Verb:                   "create",
// 			Code:                   code,
// 			User:                   "system:apiserver",
// 			ImpersonatedUser:       testReinvocationClientUsername,
// 			ImpersonatedGroups:     "system:authenticated",
// 			Resource:               "configmaps",
// 			Namespace:              ns,
// 			AuthorizeDecision:      "allow",
// 			RequestObject:          true,
// 			ResponseObject:         false,
// 			CustomAuditAnnotations: auditAnnotations,
// 		},
// 	}
// }

const (
	testReinvocationClientUsername = "webhook-reinvocation-integration-client"
	auditPolicy                    = `
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Request
    resources:
      - group: "" # core
        resources: ["configmaps"]
`
)

func TestValidatingAdmissionPolicyTypeChecking(t *testing.T) {
	client, cleanup := serverScope(nil)
	t.Cleanup(cleanup)

	for _, tc := range []struct {
		name           string
		policy         *admissionregistrationv1alpha1.ValidatingAdmissionPolicy
		assertFieldRef func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) // warning.fieldRef
		assertWarnings func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) // warning.warning
	}{
		{
			name: "deployment with correct expression",
			policy: withGVRMatch([]string{"apps"}, []string{"v1"}, []string{"deployments"}, withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.spec.replicas > 1",
				},
			}, makePolicy("replicated-deployment"))),
			assertFieldRef: toHasLengthOf(0),
			assertWarnings: toHasLengthOf(0),
		},
		{
			name: "deployment with type confusion",
			policy: withGVRMatch([]string{"apps"}, []string{"v1"}, []string{"deployments"}, withValidations([]admissionregistrationv1alpha1.Validation{
				{
					Expression: "object.spec.replicas < 100", // this one passes
				},
				{
					Expression: "object.spec.replicas > '1'", // '1' should be int
				},
			}, makePolicy("confused-deployment"))),
			assertFieldRef: toBe("spec.validations[1].expression"),
			assertWarnings: toHasSubstring(`found no matching overload for '_>_' applied to '(int, string)'`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			policy, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Create(ctx, tc.policy, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Delete(context.Background(), policy.Name, metav1.DeleteOptions{})
			err = wait.PollImmediateWithContext(ctx, time.Second, time.Minute, func(ctx context.Context) (done bool, err error) {
				name := policy.Name
				// wait until the typeChecking is set, which means the type checking
				// is complete.
				updated, err := client.AdmissionregistrationV1alpha1().ValidatingAdmissionPolicies().Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if updated.Status.TypeChecking != nil {
					policy = updated
					return true, nil
				}
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			tc.assertFieldRef(policy.Status.TypeChecking.ExpressionWarnings, t)
			tc.assertWarnings(policy.Status.TypeChecking.ExpressionWarnings, t)
		})
	}
}

func toBe(expected ...string) func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
	return func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
		if len(expected) != len(warnings) {
			t.Fatalf("mismatched length, expect %d, got %d", len(expected), len(warnings))
		}
		for i := range expected {
			if expected[i] != warnings[i].FieldRef {
				t.Errorf("expected %q but got %q", expected[i], warnings[i].FieldRef)
			}
		}
	}
}

func toHasSubstring(substrings ...string) func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
	return func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
		if len(substrings) != len(warnings) {
			t.Fatalf("mismatched length, expect %d, got %d", len(substrings), len(warnings))
		}
		for i := range substrings {
			if !strings.Contains(warnings[i].Warning, substrings[i]) {
				t.Errorf("missing expected substring %q in %v", substrings[i], warnings[i])
			}
		}
	}
}

func toHasLengthOf(n int) func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
	return func(warnings []admissionregistrationv1alpha1.ExpressionWarning, t *testing.T) {
		if n != len(warnings) {
			t.Fatalf("mismatched length, expect %d, got %d", n, len(warnings))
		}
	}
}
