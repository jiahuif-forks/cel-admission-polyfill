//go:build DEBUG

package cel_polyfill

import (
	"context"
	_ "embed"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/klog/v2"
)

//go:embed crds/celadmissionpolyfill.k8s.io_validationrulesets.yaml
var validationRuleSetsCRD string

//go:embed crds/celadmissionpolyfill.k8s.io_policytemplates.yaml
var policyTempaltesCRD string

//go:embed crds/admissionregistration.polyfill.sigs.k8s.io_validatingadmissionpolicies.yaml
var policyCRD string

//go:embed crds/admissionregistration.polyfill.sigs.k8s.io_validatingadmissionpolicybindings.yaml
var policyBindingCRD string

func DEBUG_InstallCRDs(ctx context.Context, client apiextensionsclientset.Interface) {
	klog.Info("installing CRDs")
	if err := InstallCRDs(ctx, client); err != nil {
		panic(err)
	}
	// _, err := client.Resource(schema.GroupVersionResource{
	// 	Group:    "apiextensions.k8s.io",
	// 	Version:  "v1",
	// 	Resource: "customresourcedefinitions",
	// }).
	// 	Patch(
	// 		ctx,
	// 		"validationrulesets.celadmissionpolyfill.k8s.io",
	// 		types.ApplyPatchType,
	// 		[]byte(validationRuleSetsCRD),
	// 		metav1.PatchOptions{FieldManager: "cel-polyfill-controller"},
	// 	)

	// if err != nil {
	// 	panic(err)
	// }

	// _, err = client.Resource(schema.GroupVersionResource{
	// 	Group:    "apiextensions.k8s.io",
	// 	Version:  "v1",
	// 	Resource: "customresourcedefinitions",
	// }).
	// 	Patch(
	// 		ctx,
	// 		"policytemplates.celadmissionpolyfill.k8s.io",
	// 		types.ApplyPatchType,
	// 		[]byte(policyTempaltesCRD),
	// 		metav1.PatchOptions{FieldManager: "cel-polyfill-controller"},
	// 	)

	// if err != nil {
	// 	panic(err)
	// }

	// _, err = client.Resource(schema.GroupVersionResource{
	// 	Group:    "apiextensions.k8s.io",
	// 	Version:  "v1",
	// 	Resource: "customresourcedefinitions",
	// }).
	// 	Patch(
	// 		ctx,
	// 		"validatingadmissionpolicies.admissionregistration.polyfill.sigs.k8s.io",
	// 		types.ApplyPatchType,
	// 		[]byte(policyCRD),
	// 		metav1.PatchOptions{FieldManager: "cel-polyfill-controller"},
	// 	)

	// if err != nil {
	// 	panic(err)
	// }

	// _, err = client.Resource(schema.GroupVersionResource{
	// 	Group:    "apiextensions.k8s.io",
	// 	Version:  "v1",
	// 	Resource: "customresourcedefinitions",
	// }).
	// 	Patch(
	// 		ctx,
	// 		"validatingadmissionpolicybindings.admissionregistration.polyfill.sigs.k8s.io",
	// 		types.ApplyPatchType,
	// 		[]byte(policyBindingCRD),
	// 		metav1.PatchOptions{FieldManager: "cel-polyfill-controller"},
	// 	)

	// if err != nil {
	// 	panic(err)
	// }
}
