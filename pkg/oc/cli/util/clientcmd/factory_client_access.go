package clientcmd

import (
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/kubernetes/pkg/api/legacyscheme"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	kclientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/homedir"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	kclientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/set"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/kubectl/util/transport"

	appsapiv1 "github.com/openshift/api/apps/v1"
	appsapi "github.com/openshift/origin/pkg/apps/apis/apps"
	imageapi "github.com/openshift/origin/pkg/image/apis/image"
	deploymentcmd "github.com/openshift/origin/pkg/oc/cli/deploymentconfigs"
	routegen "github.com/openshift/origin/pkg/route/generator"
)

type ring0Factory struct {
	clientConfig            kclientcmd.ClientConfig
	kubeClientAccessFactory kcmdutil.ClientAccessFactory
}

type ClientAccessFactory interface {
	kcmdutil.ClientAccessFactory
}

func NewClientAccessFactory(optionalClientConfig kclientcmd.ClientConfig) ClientAccessFactory {
	// if we call this factory construction method, we want the openshift style config loading
	kclientcmd.UseOpenShiftKubeConfigValues = true
	kclientcmd.ErrEmptyConfig = kclientcmd.NewErrConfigurationMissing()
	set.ParseDockerImageReferenceToStringFunc = ParseDockerImageReferenceToStringFunc

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	clientConfig := optionalClientConfig
	if optionalClientConfig == nil {
		clientConfig = kcmdutil.DefaultClientConfig(flags)
	}
	factory := &ring0Factory{
		clientConfig: clientConfig,
	}
	factory.kubeClientAccessFactory = kcmdutil.NewClientAccessFactoryFromDiscovery(
		flags,
		clientConfig,
		&discoveryFactory{clientConfig: clientConfig},
	)

	return factory
}

type discoveryFactory struct {
	clientConfig kclientcmd.ClientConfig
	cacheDir     string
}

func (f *discoveryFactory) BindFlags(flags *pflag.FlagSet) {
	defaultCacheDir := filepath.Join(homedir.HomeDir(), ".kube", "http-cache")
	flags.StringVar(&f.cacheDir, kcmdutil.FlagHTTPCacheDir, defaultCacheDir, "Default HTTP cache directory")
}

func (f *discoveryFactory) DiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	// Output using whatever version was negotiated in the client cache. The
	// version we decode with may not be the same as what the server requires.
	cfg, err := f.clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	// given 25 groups with one-ish version each, discovery needs to make 50 requests
	// double it just so we don't end up here again for a while.  This config is only used for discovery.
	cfg.Burst = 100

	if f.cacheDir != "" {
		wt := cfg.WrapTransport
		cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			if wt != nil {
				rt = wt(rt)
			}
			return transport.NewCacheRoundTripper(f.cacheDir, rt)
		}
	}

	// at this point we've negotiated and can get the client
	kubeClient, err := kclientset.NewForConfig(cfg)
	if err != nil {
		return nil, err

	}

	cacheDir := computeDiscoverCacheDir(filepath.Join(homedir.HomeDir(), ".kube", "cache", "discovery"), cfg.Host)
	return kcmdutil.NewCachedDiscoveryClient(newLegacyDiscoveryClient(kubeClient.Discovery().RESTClient()), cacheDir, time.Duration(10*time.Minute)), nil
}

func (f *ring0Factory) RawConfig() (clientcmdapi.Config, error) {
	return f.kubeClientAccessFactory.RawConfig()
}

func (f *ring0Factory) DiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return f.kubeClientAccessFactory.DiscoveryClient()
}

func (f *ring0Factory) KubernetesClientSet() (*kubernetes.Clientset, error) {
	return f.kubeClientAccessFactory.KubernetesClientSet()
}

func (f *ring0Factory) ClientSet() (kclientset.Interface, error) {
	return f.kubeClientAccessFactory.ClientSet()
}

func (f *ring0Factory) ClientConfig() (*restclient.Config, error) {
	return f.kubeClientAccessFactory.ClientConfig()
}

func (f *ring0Factory) BareClientConfig() (*restclient.Config, error) {
	return f.clientConfig.ClientConfig()
}

func (f *ring0Factory) RESTClient() (*restclient.RESTClient, error) {
	return f.kubeClientAccessFactory.RESTClient()
}

func (f *ring0Factory) UpdatePodSpecForObject(obj runtime.Object, fn func(*corev1.PodSpec) error) (bool, error) {
	switch t := obj.(type) {
	case *appsapi.DeploymentConfig:
		template := t.Spec.Template
		if template == nil {
			t.Spec.Template = template
			template = &kapi.PodTemplateSpec{}
		}
		if err := ConvertExteralPodSpecToInternal(fn)(&template.Spec); err != nil {
			return true, err
		}
		return true, nil

	case *appsapiv1.DeploymentConfig:
		template := t.Spec.Template
		if template == nil {
			template = &corev1.PodTemplateSpec{}
			t.Spec.Template = template
		}
		return true, fn(&template.Spec)

	default:
		return f.kubeClientAccessFactory.UpdatePodSpecForObject(obj, fn)
	}
}

func ConvertInteralPodSpecToExternal(inFn func(*kapi.PodSpec) error) func(*corev1.PodSpec) error {
	return func(specToMutate *corev1.PodSpec) error {
		internalPodSpec := &kapi.PodSpec{}
		if err := legacyscheme.Scheme.Convert(specToMutate, internalPodSpec, nil); err != nil {
			return err
		}
		if err := inFn(internalPodSpec); err != nil {
			return err
		}
		externalPodSpec := &corev1.PodSpec{}
		if err := legacyscheme.Scheme.Convert(internalPodSpec, externalPodSpec, nil); err != nil {
			return err
		}
		*specToMutate = *externalPodSpec
		return nil
	}
}

func ConvertExteralPodSpecToInternal(inFn func(*corev1.PodSpec) error) func(*kapi.PodSpec) error {
	return func(specToMutate *kapi.PodSpec) error {
		externalPodSpec := &corev1.PodSpec{}
		if err := legacyscheme.Scheme.Convert(specToMutate, externalPodSpec, nil); err != nil {
			return err
		}
		if err := inFn(externalPodSpec); err != nil {
			return err
		}
		internalPodSpec := &kapi.PodSpec{}
		if err := legacyscheme.Scheme.Convert(externalPodSpec, internalPodSpec, nil); err != nil {
			return err
		}
		*specToMutate = *internalPodSpec
		return nil
	}
}

func (f *ring0Factory) MapBasedSelectorForObject(object runtime.Object) (string, error) {
	switch t := object.(type) {
	case *appsapi.DeploymentConfig:
		return kubectl.MakeLabels(t.Spec.Selector), nil
	default:
		return f.kubeClientAccessFactory.MapBasedSelectorForObject(object)
	}
}

func (f *ring0Factory) PortsForObject(object runtime.Object) ([]string, error) {
	switch t := object.(type) {
	case *appsapi.DeploymentConfig:
		return getPorts(t.Spec.Template.Spec), nil
	default:
		return f.kubeClientAccessFactory.PortsForObject(object)
	}
}

func (f *ring0Factory) ProtocolsForObject(object runtime.Object) (map[string]string, error) {
	switch t := object.(type) {
	case *appsapi.DeploymentConfig:
		return getProtocols(t.Spec.Template.Spec), nil
	default:
		return f.kubeClientAccessFactory.ProtocolsForObject(object)
	}
}

func (f *ring0Factory) LabelsForObject(object runtime.Object) (map[string]string, error) {
	return f.kubeClientAccessFactory.LabelsForObject(object)
}

func (f *ring0Factory) FlagSet() *pflag.FlagSet {
	return f.kubeClientAccessFactory.FlagSet()
}

func (f *ring0Factory) Command(cmd *cobra.Command, showSecrets bool) string {
	return f.kubeClientAccessFactory.Command(cmd, showSecrets)
}

func (f *ring0Factory) BindFlags(flags *pflag.FlagSet) {
	f.kubeClientAccessFactory.BindFlags(flags)
}

func (f *ring0Factory) BindExternalFlags(flags *pflag.FlagSet) {
	f.kubeClientAccessFactory.BindExternalFlags(flags)
}

func (f *ring0Factory) DefaultResourceFilterFunc() kubectl.Filters {
	return f.kubeClientAccessFactory.DefaultResourceFilterFunc()
}

func (f *ring0Factory) SuggestedPodTemplateResources() []schema.GroupResource {
	return f.kubeClientAccessFactory.SuggestedPodTemplateResources()
}

func (f *ring0Factory) Pauser(info *resource.Info) ([]byte, error) {
	switch t := info.Object.(type) {
	case *appsapi.DeploymentConfig:
		if t.Spec.Paused {
			return nil, errors.New("is already paused")
		}
		t.Spec.Paused = true
		// TODO: Pause the deployer containers.
		return runtime.Encode(kcmdutil.InternalVersionJSONEncoder(), info.Object)
	default:
		return f.kubeClientAccessFactory.Pauser(info)
	}
}

func (f *ring0Factory) ResolveImage(image string) (string, error) {
	return f.kubeClientAccessFactory.ResolveImage(image)
}

func (f *ring0Factory) Resumer(info *resource.Info) ([]byte, error) {
	switch t := info.Object.(type) {
	case *appsapi.DeploymentConfig:
		if !t.Spec.Paused {
			return nil, errors.New("is not paused")
		}
		t.Spec.Paused = false
		// TODO: Resume the deployer containers.
		return runtime.Encode(kcmdutil.InternalVersionJSONEncoder(), info.Object)
	default:
		return f.kubeClientAccessFactory.Resumer(info)
	}
}

func (f *ring0Factory) DefaultNamespace() (string, bool, error) {
	return f.kubeClientAccessFactory.DefaultNamespace()
}

func DefaultGenerators(cmdName string) map[string]kubectl.Generator {
	generators := map[string]map[string]kubectl.Generator{}
	generators["run"] = map[string]kubectl.Generator{
		"deploymentconfig/v1": deploymentcmd.BasicDeploymentConfigController{},
		"run-controller/v1":   kubectl.BasicReplicationController{}, // legacy alias for run/v1
	}
	generators["expose"] = map[string]kubectl.Generator{
		"route/v1": routegen.RouteGenerator{},
	}

	return generators[cmdName]
}

func (f *ring0Factory) Generators(cmdName string) map[string]kubectl.Generator {
	originGenerators := DefaultGenerators(cmdName)
	kubeGenerators := f.kubeClientAccessFactory.Generators(cmdName)

	ret := map[string]kubectl.Generator{}
	for k, v := range kubeGenerators {
		ret[k] = v
	}
	for k, v := range originGenerators {
		ret[k] = v
	}
	return ret
}

func (f *ring0Factory) CanBeExposed(kind schema.GroupKind) error {
	if appsapi.IsKindOrLegacy("DeploymentConfig", kind) {
		return nil
	}
	return f.kubeClientAccessFactory.CanBeExposed(kind)
}

func (f *ring0Factory) CanBeAutoscaled(kind schema.GroupKind) error {
	if appsapi.IsKindOrLegacy("DeploymentConfig", kind) {
		return nil
	}
	return f.kubeClientAccessFactory.CanBeAutoscaled(kind)
}

func (f *ring0Factory) EditorEnvs() []string {
	return []string{"OC_EDITOR", "EDITOR"}
}

// computeDiscoverCacheDir takes the parentDir and the host and comes up with a "usually non-colliding" name.
func computeDiscoverCacheDir(parentDir, host string) string {
	// strip the optional scheme from host if its there:
	schemelessHost := strings.Replace(strings.Replace(host, "https://", "", 1), "http://", "", 1)
	// now do a simple collapse of non-AZ09 characters.  Collisions are possible but unlikely.  Even if we do collide the problem is short lived
	safeHost := overlyCautiousIllegalFileCharacters.ReplaceAllString(schemelessHost, "_")

	return filepath.Join(parentDir, safeHost)
}

// overlyCautiousIllegalFileCharacters matches characters that *might* not be supported.  Windows is really restrictive, so this is really restrictive
var overlyCautiousIllegalFileCharacters = regexp.MustCompile(`[^(\w/\.)]`)

func getPorts(spec kapi.PodSpec) []string {
	result := []string{}
	for _, container := range spec.Containers {
		for _, port := range container.Ports {
			result = append(result, strconv.Itoa(int(port.ContainerPort)))
		}
	}
	return result
}

func getProtocols(spec kapi.PodSpec) map[string]string {
	result := make(map[string]string)
	for _, container := range spec.Containers {
		for _, port := range container.Ports {
			result[strconv.Itoa(int(port.ContainerPort))] = string(port.Protocol)
		}
	}
	return result
}

func ParseDockerImageReferenceToStringFunc(spec string) (string, error) {
	ret, err := imageapi.ParseDockerImageReference(spec)
	if err != nil {
		return "", err
	}
	return ret.String(), nil
}
