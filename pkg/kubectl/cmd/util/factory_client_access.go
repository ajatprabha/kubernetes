/*
Copyright 2016 The Kubernetes Authors.

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

// this file contains factories with no other dependencies

package util

import (
	"fmt"
	"io"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	batchv2alpha1 "k8s.io/api/batch/v2alpha1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi"
	openapivalidation "k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi/validation"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions/resource"
	"k8s.io/kubernetes/pkg/kubectl/validation"
)

type factoryImpl struct {
	clientGetter genericclioptions.RESTClientGetter

	// openAPIGetter loads and caches openapi specs
	openAPIGetter openAPIGetter
}

type openAPIGetter struct {
	once   sync.Once
	getter openapi.Getter
}

func NewFactory(clientGetter genericclioptions.RESTClientGetter) Factory {
	if clientGetter == nil {
		panic("attempt to instantiate client_access_factory with nil clientGetter")
	}

	f := &factoryImpl{
		clientGetter: clientGetter,
	}

	return f
}

func (f *factoryImpl) ToRESTConfig() (*restclient.Config, error) {
	return f.clientGetter.ToRESTConfig()
}

func (f *factoryImpl) ToRESTMapper() (meta.RESTMapper, error) {
	return f.clientGetter.ToRESTMapper()
}

func (f *factoryImpl) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return f.clientGetter.ToDiscoveryClient()
}

func (f *factoryImpl) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return f.clientGetter.ToRawKubeConfigLoader()
}

func (f *factoryImpl) KubernetesClientSet() (*kubernetes.Clientset, error) {
	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(clientConfig)
}

func (f *factoryImpl) ClientSet() (internalclientset.Interface, error) {
	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	return internalclientset.NewForConfig(clientConfig)
}

func (f *factoryImpl) DynamicClient() (dynamic.Interface, error) {
	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(clientConfig)
}

// NewBuilder returns a new resource builder for structured api objects.
func (f *factoryImpl) NewBuilder() *resource.Builder {
	return resource.NewBuilder(f.clientGetter)
}

func (f *factoryImpl) RESTClient() (*restclient.RESTClient, error) {
	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	setKubernetesDefaults(clientConfig)
	return restclient.RESTClientFor(clientConfig)
}

func (f *factoryImpl) DefaultNamespace() (string, bool, error) {
	return f.clientGetter.ToRawKubeConfigLoader().Namespace()
}

func (f *factoryImpl) ClientForMapping(mapping *meta.RESTMapping) (resource.RESTClient, error) {
	cfg, err := f.clientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	if err := setKubernetesDefaults(cfg); err != nil {
		return nil, err
	}
	gvk := mapping.GroupVersionKind
	switch gvk.Group {
	case api.GroupName:
		cfg.APIPath = "/api"
	default:
		cfg.APIPath = "/apis"
	}
	gv := gvk.GroupVersion()
	cfg.GroupVersion = &gv
	return restclient.RESTClientFor(cfg)
}

func (f *factoryImpl) UnstructuredClientForMapping(mapping *meta.RESTMapping) (resource.RESTClient, error) {
	cfg, err := f.clientGetter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	if err := restclient.SetKubernetesDefaults(cfg); err != nil {
		return nil, err
	}
	cfg.APIPath = "/apis"
	if mapping.GroupVersionKind.Group == api.GroupName {
		cfg.APIPath = "/api"
	}
	gv := mapping.GroupVersionKind.GroupVersion()
	cfg.ContentConfig = resource.UnstructuredPlusDefaultContentConfig()
	cfg.GroupVersion = &gv
	return restclient.RESTClientFor(cfg)
}

func (f *factoryImpl) Validator(validate bool) (validation.Schema, error) {
	if !validate {
		return validation.NullSchema{}, nil
	}

	resources, err := f.OpenAPISchema()
	if err != nil {
		return nil, err
	}

	return validation.ConjunctiveSchema{
		openapivalidation.NewSchemaValidation(resources),
		validation.NoDoubleKeySchema{},
	}, nil
}

// OpenAPISchema returns metadata and structural information about Kubernetes object definitions.
func (f *factoryImpl) OpenAPISchema() (openapi.Resources, error) {
	discovery, err := f.clientGetter.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	// Lazily initialize the OpenAPIGetter once
	f.openAPIGetter.once.Do(func() {
		// Create the caching OpenAPIGetter
		f.openAPIGetter.getter = openapi.NewOpenAPIGetter(discovery)
	})

	// Delegate to the OpenAPIGetter
	return f.openAPIGetter.getter.Get()
}

func (f *factoryImpl) ScaleClient() (scaleclient.ScalesGetter, error) {
	discoClient, err := f.clientGetter.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	restClient, err := f.RESTClient()
	if err != nil {
		return nil, err
	}
	resolver := scaleclient.NewDiscoveryScaleKindResolver(discoClient)
	mapper, err := f.clientGetter.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	return scaleclient.New(restClient, mapper, dynamic.LegacyAPIPathResolverFunc, resolver), nil
}

const (
	// TODO(sig-cli): Enforce consistent naming for generators here.
	// See discussion in https://github.com/kubernetes/kubernetes/issues/46237
	// before you add any more.
	RunV1GeneratorName                      = "run/v1"
	RunPodV1GeneratorName                   = "run-pod/v1"
	ServiceV1GeneratorName                  = "service/v1"
	ServiceV2GeneratorName                  = "service/v2"
	ServiceNodePortGeneratorV1Name          = "service-nodeport/v1"
	ServiceClusterIPGeneratorV1Name         = "service-clusterip/v1"
	ServiceLoadBalancerGeneratorV1Name      = "service-loadbalancer/v1"
	ServiceExternalNameGeneratorV1Name      = "service-externalname/v1"
	ServiceAccountV1GeneratorName           = "serviceaccount/v1"
	HorizontalPodAutoscalerV1GeneratorName  = "horizontalpodautoscaler/v1"
	DeploymentV1Beta1GeneratorName          = "deployment/v1beta1"
	DeploymentAppsV1Beta1GeneratorName      = "deployment/apps.v1beta1"
	DeploymentBasicV1Beta1GeneratorName     = "deployment-basic/v1beta1"
	DeploymentBasicAppsV1Beta1GeneratorName = "deployment-basic/apps.v1beta1"
	DeploymentBasicAppsV1GeneratorName      = "deployment-basic/apps.v1"
	JobV1GeneratorName                      = "job/v1"
	CronJobV2Alpha1GeneratorName            = "cronjob/v2alpha1"
	CronJobV1Beta1GeneratorName             = "cronjob/v1beta1"
	NamespaceV1GeneratorName                = "namespace/v1"
	ResourceQuotaV1GeneratorName            = "resourcequotas/v1"
	SecretV1GeneratorName                   = "secret/v1"
	SecretForDockerRegistryV1GeneratorName  = "secret-for-docker-registry/v1"
	SecretForTLSV1GeneratorName             = "secret-for-tls/v1"
	ConfigMapV1GeneratorName                = "configmap/v1"
	ClusterRoleBindingV1GeneratorName       = "clusterrolebinding.rbac.authorization.k8s.io/v1alpha1"
	RoleBindingV1GeneratorName              = "rolebinding.rbac.authorization.k8s.io/v1alpha1"
	ClusterV1Beta1GeneratorName             = "cluster/v1beta1"
	PodDisruptionBudgetV1GeneratorName      = "poddisruptionbudget/v1beta1"
	PodDisruptionBudgetV2GeneratorName      = "poddisruptionbudget/v1beta1/v2"
	PriorityClassV1Alpha1GeneratorName      = "priorityclass/v1alpha1"
)

// DefaultGenerators returns the set of default generators for use in Factory instances
func DefaultGenerators(cmdName string) map[string]kubectl.Generator {
	var generator map[string]kubectl.Generator
	switch cmdName {
	case "expose":
		generator = map[string]kubectl.Generator{
			ServiceV1GeneratorName: kubectl.ServiceGeneratorV1{},
			ServiceV2GeneratorName: kubectl.ServiceGeneratorV2{},
		}
	case "service-clusterip":
		generator = map[string]kubectl.Generator{
			ServiceClusterIPGeneratorV1Name: kubectl.ServiceClusterIPGeneratorV1{},
		}
	case "service-nodeport":
		generator = map[string]kubectl.Generator{
			ServiceNodePortGeneratorV1Name: kubectl.ServiceNodePortGeneratorV1{},
		}
	case "service-loadbalancer":
		generator = map[string]kubectl.Generator{
			ServiceLoadBalancerGeneratorV1Name: kubectl.ServiceLoadBalancerGeneratorV1{},
		}
	case "deployment":
		// Create Deployment has only StructuredGenerators and no
		// param-based Generators.
		// The StructuredGenerators are as follows (as of 2018-03-16):
		// DeploymentBasicV1Beta1GeneratorName -> kubectl.DeploymentBasicGeneratorV1
		// DeploymentBasicAppsV1Beta1GeneratorName -> kubectl.DeploymentBasicAppsGeneratorV1Beta1
		// DeploymentBasicAppsV1GeneratorName -> kubectl.DeploymentBasicAppsGeneratorV1
		generator = map[string]kubectl.Generator{}
	case "run":
		generator = map[string]kubectl.Generator{
			RunV1GeneratorName:                 kubectl.BasicReplicationController{},
			RunPodV1GeneratorName:              kubectl.BasicPod{},
			DeploymentV1Beta1GeneratorName:     kubectl.DeploymentV1Beta1{},
			DeploymentAppsV1Beta1GeneratorName: kubectl.DeploymentAppsV1Beta1{},
			JobV1GeneratorName:                 kubectl.JobV1{},
			CronJobV2Alpha1GeneratorName:       kubectl.CronJobV2Alpha1{},
			CronJobV1Beta1GeneratorName:        kubectl.CronJobV1Beta1{},
		}
	case "namespace":
		generator = map[string]kubectl.Generator{
			NamespaceV1GeneratorName: kubectl.NamespaceGeneratorV1{},
		}
	case "quota":
		generator = map[string]kubectl.Generator{
			ResourceQuotaV1GeneratorName: kubectl.ResourceQuotaGeneratorV1{},
		}
	case "secret":
		generator = map[string]kubectl.Generator{
			SecretV1GeneratorName: kubectl.SecretGeneratorV1{},
		}
	case "secret-for-docker-registry":
		generator = map[string]kubectl.Generator{
			SecretForDockerRegistryV1GeneratorName: kubectl.SecretForDockerRegistryGeneratorV1{},
		}
	case "secret-for-tls":
		generator = map[string]kubectl.Generator{
			SecretForTLSV1GeneratorName: kubectl.SecretForTLSGeneratorV1{},
		}
	}

	return generator
}

// fallbackGeneratorNameIfNecessary returns the name of the old generator
// if server does not support new generator. Otherwise, the
// generator string is returned unchanged.
//
// If the generator name is changed, print a warning message to let the user
// know.
func FallbackGeneratorNameIfNecessary(
	generatorName string,
	discoveryClient discovery.DiscoveryInterface,
	cmdErr io.Writer,
) (string, error) {
	switch generatorName {
	case DeploymentAppsV1Beta1GeneratorName:
		hasResource, err := HasResource(discoveryClient, appsv1beta1.SchemeGroupVersion.WithResource("deployments"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return FallbackGeneratorNameIfNecessary(DeploymentV1Beta1GeneratorName, discoveryClient, cmdErr)
		}
	case DeploymentV1Beta1GeneratorName:
		hasResource, err := HasResource(discoveryClient, extensionsv1beta1.SchemeGroupVersion.WithResource("deployments"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return RunV1GeneratorName, nil
		}
	case DeploymentBasicAppsV1GeneratorName:
		hasResource, err := HasResource(discoveryClient, appsv1.SchemeGroupVersion.WithResource("deployments"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return FallbackGeneratorNameIfNecessary(DeploymentBasicAppsV1Beta1GeneratorName, discoveryClient, cmdErr)
		}
	case DeploymentBasicAppsV1Beta1GeneratorName:
		hasResource, err := HasResource(discoveryClient, appsv1beta1.SchemeGroupVersion.WithResource("deployments"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return DeploymentBasicV1Beta1GeneratorName, nil
		}
	case JobV1GeneratorName:
		hasResource, err := HasResource(discoveryClient, batchv1.SchemeGroupVersion.WithResource("jobs"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return RunPodV1GeneratorName, nil
		}
	case CronJobV1Beta1GeneratorName:
		hasResource, err := HasResource(discoveryClient, batchv1beta1.SchemeGroupVersion.WithResource("cronjobs"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return FallbackGeneratorNameIfNecessary(CronJobV2Alpha1GeneratorName, discoveryClient, cmdErr)
		}
	case CronJobV2Alpha1GeneratorName:
		hasResource, err := HasResource(discoveryClient, batchv2alpha1.SchemeGroupVersion.WithResource("cronjobs"))
		if err != nil {
			return "", err
		}
		if !hasResource {
			return JobV1GeneratorName, nil
		}
	}
	return generatorName, nil
}

func Warning(cmdErr io.Writer, newGeneratorName, oldGeneratorName string) {
	fmt.Fprintf(cmdErr, "WARNING: New generator %q specified, "+
		"but it isn't available. "+
		"Falling back to %q.\n",
		newGeneratorName,
		oldGeneratorName,
	)
}

func HasResource(client discovery.DiscoveryInterface, resource schema.GroupVersionResource) (bool, error) {
	resources, err := client.ServerResourcesForGroupVersion(resource.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		// entire group is missing
		return false, nil
	}
	if err != nil {
		// other errors error
		return false, fmt.Errorf("failed to discover supported resources: %v", err)
	}
	for _, serverResource := range resources.APIResources {
		if serverResource.Name == resource.Resource {
			return true, nil
		}
	}
	return false, nil
}

func Contains(resourcesList []*metav1.APIResourceList, resource schema.GroupVersionResource) bool {
	resources := discovery.FilteredBy(discovery.ResourcePredicateFunc(func(gv string, r *metav1.APIResource) bool {
		return resource.GroupVersion().String() == gv && resource.Resource == r.Name
	}), resourcesList)
	return len(resources) != 0
}

func (f *factoryImpl) Generators(cmdName string) map[string]kubectl.Generator {
	return DefaultGenerators(cmdName)
}

// this method exists to help us find the points still relying on internal types.
func InternalVersionDecoder() runtime.Decoder {
	return legacyscheme.Codecs.UniversalDecoder()
}

func InternalVersionJSONEncoder() runtime.Encoder {
	encoder := legacyscheme.Codecs.LegacyCodec(legacyscheme.Scheme.PrioritizedVersionsAllGroups()...)
	return unstructured.JSONFallbackEncoder{Encoder: encoder}
}
