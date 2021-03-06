package url

import (
	"fmt"
	"github.com/openshift/odo/pkg/localConfigProvider"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/devfile/library/pkg/devfile/generator"

	"github.com/gosimple/slug"
	"github.com/openshift/odo/pkg/envinfo"
	"github.com/openshift/odo/pkg/log"

	devfilev1 "github.com/devfile/api/pkg/apis/workspaces/v1alpha2"
	types "github.com/docker/docker/api/types"
	routev1 "github.com/openshift/api/route/v1"
	applabels "github.com/openshift/odo/pkg/application/labels"
	componentlabels "github.com/openshift/odo/pkg/component/labels"
	"github.com/openshift/odo/pkg/config"
	dockercomponent "github.com/openshift/odo/pkg/devfile/adapters/docker/component"
	dockerutils "github.com/openshift/odo/pkg/devfile/adapters/docker/utils"
	"github.com/openshift/odo/pkg/kclient"
	"github.com/openshift/odo/pkg/lclient"
	"github.com/openshift/odo/pkg/occlient"
	urlLabels "github.com/openshift/odo/pkg/url/labels"
	"github.com/openshift/odo/pkg/util"
	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"

	iextensionsv1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"
)

const apiVersion = "odo.dev/v1alpha1"

// Get returns URL definition for given URL name
func (urls URLList) Get(urlName string) URL {
	for _, url := range urls.Items {
		if url.Name == urlName {
			return url
		}
	}
	return URL{}

}

// GetIngressOrRoute returns ingress/route spec for given URL name
func GetIngressOrRoute(client *occlient.Client, kClient *kclient.Client, envSpecificInfo *envinfo.EnvSpecificInfo, urlName string, containerComponents []devfilev1.Component, componentName string, routeSupported bool) (URL, error) {
	remoteExist := true
	var ingress *iextensionsv1.Ingress
	var route *routev1.Route
	var getRouteErr error

	// route/ingress name is defined as <urlName>-<componentName>
	// to avoid error due to duplicate ingress name defined in different devfile components
	trimmedURLName := getValidURLName(urlName)
	remoteURLName := fmt.Sprintf("%s-%s", trimmedURLName, componentName)
	// Check whether remote already created the ingress
	ingress, getIngressErr := kClient.GetIngress(remoteURLName)
	if kerrors.IsNotFound(getIngressErr) && routeSupported {
		// Check whether remote already created the route
		route, getRouteErr = client.GetRoute(remoteURLName)
	}
	if kerrors.IsNotFound(getIngressErr) && (!routeSupported || kerrors.IsNotFound(getRouteErr)) {
		remoteExist = false
	} else if (getIngressErr != nil && !kerrors.IsNotFound(getIngressErr)) || (getRouteErr != nil && !kerrors.IsNotFound(getRouteErr)) {
		if getIngressErr != nil {
			return URL{}, errors.Wrap(getIngressErr, "unable to get ingress")
		}
		return URL{}, errors.Wrap(getRouteErr, "unable to get route")
	}

	envinfoURLs := envSpecificInfo.ListURLs()
	envURLMap := make(map[string]localConfigProvider.LocalURL)
	for _, url := range envinfoURLs {
		envURLMap[url.Name] = url
	}

	for _, comp := range containerComponents {
		for _, localEndpoint := range comp.Container.Endpoints {
			if localEndpoint.Name != urlName {
				continue
			}

			if localEndpoint.Exposure == devfilev1.NoneEndpointExposure || localEndpoint.Exposure == devfilev1.InternalEndpointExposure {
				return URL{}, errors.New(fmt.Sprintf("the url %v is defined in devfile, but is not exposed", urlName))
			}
			var devfileURL localConfigProvider.LocalURL
			if envinfoURL, exist := envURLMap[localEndpoint.Name]; exist {
				if envinfoURL.Kind == localConfigProvider.DOCKER {
					return URL{}, errors.New(fmt.Sprintf("the url %v is defined with type of Docker", urlName))
				}
				if !routeSupported && envinfoURL.Kind == localConfigProvider.ROUTE {
					return URL{}, errors.New(fmt.Sprintf("the url %v is defined with type of Route, but Route is not support in current cluster", urlName))
				}
				devfileURL = envinfoURL
				devfileURL.Port = int(localEndpoint.TargetPort)
				devfileURL.Secure = localEndpoint.Secure
			}
			if reflect.DeepEqual(devfileURL, localConfigProvider.LocalURL{}) {
				// Devfile endpoint by default should create a route if no host information is provided in env.yaml
				// If it is not openshift cluster, should ignore the endpoint entry when executing url describe/list
				if !routeSupported {
					break
				}
				devfileURL.Name = urlName
				devfileURL.Port = int(localEndpoint.TargetPort)
				devfileURL.Secure = localEndpoint.Secure
				devfileURL.Kind = localConfigProvider.ROUTE
			}
			localURL := ConvertEnvinfoURL(devfileURL, componentName)
			if remoteExist {
				if ingress != nil && ingress.Spec.Rules != nil {
					// Remote exist, but not in local, so it's deleted status
					clusterURL := getMachineReadableFormatIngress(*ingress)
					clusterURL.Status.State = StateTypePushed
					return clusterURL, nil
				} else if route != nil {
					clusterURL := getMachineReadableFormat(*route)
					clusterURL.Status.State = StateTypePushed
					return clusterURL, nil
				}
			} else {
				localURL.Status.State = StateTypeNotPushed
			}
			return localURL, nil
		}
	}

	if remoteExist {
		if ingress != nil && ingress.Spec.Rules != nil {
			// Remote exist, but not in local, so it's deleted status
			clusterURL := getMachineReadableFormatIngress(*ingress)
			clusterURL.Status.State = StateTypeLocallyDeleted
			return clusterURL, nil
		} else if route != nil {
			clusterURL := getMachineReadableFormat(*route)
			clusterURL.Status.State = StateTypeLocallyDeleted
			return clusterURL, nil
		}
	}

	// can't find the URL in local and remote
	return URL{}, errors.New(fmt.Sprintf("the url %v does not exist", urlName))
}

// Delete deletes a URL
func Delete(client *occlient.Client, kClient *kclient.Client, urlName string, applicationName string, urlType localConfigProvider.URLKind, isS2i bool) error {
	if urlType == localConfigProvider.INGRESS {
		return kClient.DeleteIngress(urlName)
	} else if urlType == localConfigProvider.ROUTE {
		if isS2i {
			// Namespace the URL name
			var err error
			urlName, err = util.NamespaceOpenShiftObject(urlName, applicationName)
			if err != nil {
				return errors.Wrapf(err, "unable to create namespaced name")
			}
		}

		return client.DeleteRoute(urlName)
	}
	return errors.New("url type is not supported")
}

type CreateParameters struct {
	urlName         string
	portNumber      int
	secureURL       bool
	componentName   string
	applicationName string
	host            string
	secretName      string
	urlKind         localConfigProvider.URLKind
	path            string
}

// Create creates a URL and returns url string and error if any
// portNumber is the target port number for the route and is -1 in case no port number is specified in which case it is automatically detected for components which expose only one service port)
func Create(client *occlient.Client, kClient *kclient.Client, parameters CreateParameters, isRouteSupported bool, isS2I bool) (string, error) {

	if parameters.urlKind != localConfigProvider.INGRESS && parameters.urlKind != localConfigProvider.ROUTE {
		return "", fmt.Errorf("urlKind %s is not supported for URL creation", parameters.urlKind)
	}

	if !parameters.secureURL && parameters.secretName != "" {
		return "", fmt.Errorf("secret name can only be used for secure URLs")
	}

	labels := urlLabels.GetLabels(parameters.urlName, parameters.componentName, parameters.applicationName, true)

	serviceName := ""

	if !isS2I && parameters.urlKind == localConfigProvider.INGRESS && kClient != nil {
		if parameters.host == "" {
			return "", errors.Errorf("the host cannot be empty")
		}
		serviceName := parameters.componentName
		ingressDomain := fmt.Sprintf("%v.%v", parameters.urlName, parameters.host)
		deployment, err := kClient.GetDeploymentByName(parameters.componentName)
		if err != nil {
			return "", err
		}
		ownerReference := generator.GetOwnerReference(deployment)
		if parameters.secureURL {
			if len(parameters.secretName) != 0 {
				_, err := kClient.KubeClient.CoreV1().Secrets(kClient.Namespace).Get(parameters.secretName, metav1.GetOptions{})
				if err != nil {
					return "", errors.Wrap(err, "unable to get the provided secret: "+parameters.secretName)
				}
			}
			if len(parameters.secretName) == 0 {
				defaultTLSSecretName := parameters.componentName + "-tlssecret"
				_, err := kClient.KubeClient.CoreV1().Secrets(kClient.Namespace).Get(defaultTLSSecretName, metav1.GetOptions{})
				// create tls secret if it does not exist
				if kerrors.IsNotFound(err) {
					selfsignedcert, err := kclient.GenerateSelfSignedCertificate(parameters.host)
					if err != nil {
						return "", errors.Wrap(err, "unable to generate self-signed certificate for clutser: "+parameters.host)
					}
					// create tls secret
					secretlabels := componentlabels.GetLabels(parameters.componentName, parameters.applicationName, true)
					objectMeta := metav1.ObjectMeta{
						Name:   defaultTLSSecretName,
						Labels: secretlabels,
						OwnerReferences: []v1.OwnerReference{
							ownerReference,
						},
					}
					secret, err := kClient.CreateTLSSecret(selfsignedcert.CertPem, selfsignedcert.KeyPem, objectMeta)
					if err != nil {
						return "", errors.Wrap(err, "unable to create tls secret")
					}
					parameters.secretName = secret.Name
				} else if err != nil {
					return "", err
				} else {
					// tls secret found for this component
					parameters.secretName = defaultTLSSecretName
				}

			}

		}

		objectMeta := generator.GetObjectMeta(parameters.componentName, kClient.Namespace, labels, nil)
		// to avoid error due to duplicate ingress name defined in different devfile components
		objectMeta.Name = fmt.Sprintf("%s-%s", parameters.urlName, parameters.componentName)
		objectMeta.OwnerReferences = append(objectMeta.OwnerReferences, ownerReference)

		ingressParam := generator.IngressParams{
			ObjectMeta: objectMeta,
			IngressSpecParams: generator.IngressSpecParams{
				ServiceName:   serviceName,
				IngressDomain: ingressDomain,
				PortNumber:    intstr.FromInt(parameters.portNumber),
				TLSSecretName: parameters.secretName,
				Path:          parameters.path,
			},
		}
		ingress := generator.GetIngress(ingressParam)
		// Pass in the namespace name, link to the service (componentName) and labels to create a ingress
		i, err := kClient.CreateIngress(*ingress)
		if err != nil {
			return "", errors.Wrap(err, "unable to create ingress")
		}
		return GetURLString(GetProtocol(routev1.Route{}, *i), "", ingressDomain, false), nil
	} else {
		if !isRouteSupported {
			return "", errors.Errorf("routes are not available on non OpenShift clusters")
		}

		var ownerReference metav1.OwnerReference
		if isS2I || kClient == nil {
			var err error
			parameters.urlName, err = util.NamespaceOpenShiftObject(parameters.urlName, parameters.applicationName)
			if err != nil {
				return "", errors.Wrapf(err, "unable to create namespaced name")
			}
			serviceName, err = util.NamespaceOpenShiftObject(parameters.componentName, parameters.applicationName)
			if err != nil {
				return "", errors.Wrapf(err, "unable to create namespaced name")
			}

			// since the serviceName is same as the DC name, we use that to get the DC
			// to which this route belongs. A better way could be to get service from
			// the name and set it as owner of the route
			dc, err := client.GetDeploymentConfigFromName(serviceName)
			if err != nil {
				return "", errors.Wrapf(err, "unable to get DeploymentConfig %s", serviceName)
			}

			ownerReference = occlient.GenerateOwnerReference(dc)
		} else {
			// to avoid error due to duplicate ingress name defined in different devfile components
			parameters.urlName = fmt.Sprintf("%s-%s", parameters.urlName, parameters.componentName)
			serviceName = parameters.componentName

			deployment, err := kClient.GetDeploymentByName(parameters.componentName)
			if err != nil {
				return "", err
			}
			ownerReference = generator.GetOwnerReference(deployment)
		}

		// Pass in the namespace name, link to the service (componentName) and labels to create a route
		route, err := client.CreateRoute(parameters.urlName, serviceName, intstr.FromInt(parameters.portNumber), labels, parameters.secureURL, parameters.path, ownerReference)
		if err != nil {
			return "", errors.Wrap(err, "unable to create route")
		}
		return GetURLString(GetProtocol(*route, iextensionsv1.Ingress{}), route.Spec.Host, "", true), nil
	}

}

// ListPushed lists the URLs in an application that are in cluster. The results can further be narrowed
// down if a component name is provided, which will only list URLs for the
// given component
func ListPushed(client *occlient.Client, componentName string, applicationName string) (URLList, error) {

	labelSelector := fmt.Sprintf("%v=%v", applabels.ApplicationLabel, applicationName)

	if componentName != "" {
		labelSelector = labelSelector + fmt.Sprintf(",%v=%v", componentlabels.ComponentLabel, componentName)
	}

	klog.V(4).Infof("Listing routes with label selector: %v", labelSelector)
	routes, err := client.ListRoutes(labelSelector)

	if err != nil {
		return URLList{}, errors.Wrap(err, "unable to list route names")
	}

	var urls []URL
	for _, r := range routes {
		if r.OwnerReferences != nil && r.OwnerReferences[0].Kind == "Ingress" {
			continue
		}
		a := getMachineReadableFormat(r)
		urls = append(urls, a)
	}

	urlList := getMachineReadableFormatForList(urls)
	return urlList, nil

}

// ListPushedIngress lists the ingress URLs on cluster for the given component
func ListPushedIngress(client *kclient.Client, componentName string) (URLList, error) {
	labelSelector := fmt.Sprintf("%v=%v", componentlabels.ComponentLabel, componentName)
	klog.V(4).Infof("Listing ingresses with label selector: %v", labelSelector)
	ingresses, err := client.ListIngresses(labelSelector)
	if err != nil {
		return URLList{}, errors.Wrap(err, "unable to list ingress names")
	}

	var urls []URL
	for _, i := range ingresses {
		a := getMachineReadableFormatIngress(i)
		urls = append(urls, a)
	}

	urlList := getMachineReadableFormatForList(urls)
	return urlList, nil
}

// List returns all URLs for given component.
// If componentName is empty string, it lists all url in a given application.
func List(client *occlient.Client, localConfig *config.LocalConfigInfo, componentName string, applicationName string) (URLList, error) {

	labelSelector := fmt.Sprintf("%v=%v", applabels.ApplicationLabel, applicationName)

	if componentName != "" {
		labelSelector = labelSelector + fmt.Sprintf(",%v=%v", componentlabels.ComponentLabel, componentName)
	}

	routes, err := client.ListRoutes(labelSelector)
	if err != nil {
		return URLList{}, errors.Wrap(err, "unable to list route names")
	}

	localConfigURLs := localConfig.ListURLs()

	var urls []URL

	for _, r := range routes {
		clusterURL := getMachineReadableFormat(r)
		var found bool = false
		for _, configURL := range localConfigURLs {
			localURL := ConvertConfigURL(configURL)
			if localURL.Name == clusterURL.Name {
				// URL is in both local config and cluster
				clusterURL.Status.State = StateTypePushed
				urls = append(urls, clusterURL)
				found = true
			}
		}

		if !found {
			// URL is on the cluster but not in local config
			clusterURL.Status.State = StateTypeLocallyDeleted
			urls = append(urls, clusterURL)
		}
	}

	for _, configURL := range localConfigURLs {
		localURL := ConvertConfigURL(configURL)
		var found = false
		for _, r := range routes {
			clusterURL := getMachineReadableFormat(r)
			if localURL.Name == clusterURL.Name {
				found = true
			}
		}
		if !found {
			// URL is in the local config but not on the cluster
			localURL.Status.State = StateTypeNotPushed
			urls = append(urls, localURL)
		}
	}

	urlList := getMachineReadableFormatForList(urls)
	return urlList, nil
}

type sortableURLs []URL

func (s sortableURLs) Len() int {
	return len(s)
}

func (s sortableURLs) Less(i, j int) bool {
	return s[i].Name <= s[j].Name
}

func (s sortableURLs) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// ListIngressAndRoute returns all Ingress and Route for given component.
func ListIngressAndRoute(oclient *occlient.Client, configProvider localConfigProvider.LocalConfigProvider, containerComponents []devfilev1.Component, componentName string, routeSupported bool) (URLList, error) {
	labelSelector := fmt.Sprintf("%v=%v", componentlabels.ComponentLabel, componentName)
	klog.V(4).Infof("Listing ingresses with label selector: %v", labelSelector)
	ingresses, err := oclient.GetKubeClient().ListIngresses(labelSelector)
	if err != nil {
		return URLList{}, errors.Wrap(err, "unable to list ingress")
	}
	routes := []routev1.Route{}
	if routeSupported {
		routes, err = oclient.ListRoutes(labelSelector)
		if err != nil {
			return URLList{}, errors.Wrap(err, "unable to list routes")
		}
	}

	envURLMap := make(map[string]localConfigProvider.LocalURL)
	if configProvider != nil {
		localEnvinfoURLs := configProvider.ListURLs()
		for _, url := range localEnvinfoURLs {
			if url.Kind == localConfigProvider.DOCKER {
				continue
			}
			if !routeSupported && url.Kind == localConfigProvider.ROUTE {
				continue
			}
			envURLMap[url.Name] = url
		}
	}

	var urls sortableURLs

	clusterURLMap := make(map[string]URL)
	localMap := make(map[string]URL)
	for _, i := range ingresses {
		clusterURL := getMachineReadableFormatIngress(i)
		clusterURLMap[clusterURL.Name] = clusterURL
	}
	for _, r := range routes {
		if r.OwnerReferences != nil && r.OwnerReferences[0].Kind == "Ingress" {
			continue
		}
		clusterURL := getMachineReadableFormat(r)
		clusterURLMap[clusterURL.Name] = clusterURL
	}

	if len(containerComponents) > 0 {
		for _, comp := range containerComponents {
			for _, localEndpoint := range comp.Container.Endpoints {
				// only exposed endpoint will be shown as a URL in `odo url list`
				if localEndpoint.Exposure == devfilev1.NoneEndpointExposure || localEndpoint.Exposure == devfilev1.InternalEndpointExposure {
					continue
				}
				var devfileURL localConfigProvider.LocalURL
				if envinfoURL, exist := envURLMap[localEndpoint.Name]; exist {
					devfileURL = envinfoURL
					devfileURL.Port = int(localEndpoint.TargetPort)
					devfileURL.Secure = localEndpoint.Secure
				}
				if reflect.DeepEqual(devfileURL, localConfigProvider.LocalURL{}) {
					// Devfile endpoint by default should create a route if no host information is provided in env.yaml
					// If it is not openshift cluster, should ignore the endpoint entry when executing url describe/list
					if !routeSupported {
						continue
					}
					devfileURL.Name = localEndpoint.Name
					devfileURL.Port = int(localEndpoint.TargetPort)
					devfileURL.Secure = localEndpoint.Secure
					devfileURL.Kind = localConfigProvider.ROUTE
				}
				localURL := ConvertEnvinfoURL(devfileURL, componentName)
				// use the trimmed URL Name as the key since remote URLs' names are trimmed
				trimmedURLName := getValidURLName(localURL.Name)
				localMap[trimmedURLName] = localURL
			}
		}
	} else {
		for _, url := range envURLMap {
			localURL := ConvertEnvinfoURL(url, componentName)
			// use the trimmed URL Name as the key since remote URLs' names are trimmed
			trimmedURLName := getValidURLName(localURL.Name)
			localMap[trimmedURLName] = localURL
		}
	}

	for URLName, clusterURL := range clusterURLMap {
		_, found := localMap[URLName]
		if found {
			// URL is in both local env file and cluster
			clusterURL.Status.State = StateTypePushed
			urls = append(urls, clusterURL)
		} else {
			// URL is on the cluster but not in local env file
			clusterURL.Status.State = StateTypeLocallyDeleted
			urls = append(urls, clusterURL)
		}
	}

	for localName, localURL := range localMap {
		_, remoteURLFound := clusterURLMap[localName]
		if !remoteURLFound {
			// URL is in the local env file but not pushed to cluster
			localURL.Status.State = StateTypeNotPushed
			urls = append(urls, localURL)
		}
	}

	// sort urls by name to get consistent output
	sort.Sort(urls)
	urlList := getMachineReadableFormatForList(urls)
	return urlList, nil
}

// ListDockerURL returns all Docker URLs for given component.
func ListDockerURL(client *lclient.Client, componentName string, envSpecificInfo *envinfo.EnvSpecificInfo) (URLList, error) {
	containers, err := dockerutils.GetComponentContainers(*client, componentName)
	if err != nil {
		return URLList{}, errors.Wrap(err, "unable to list component container")
	}

	localURLs := envSpecificInfo.ListURLs()

	var urls []URL

	containerJSONMap := make(map[string]types.ContainerJSON)

	// iterating through each container's HostConfig
	// find out if there is a match config in local env.yaml
	// if found a match, then the URL is <Pushed>
	// if the config does not exist in local env.yaml file, then the URL is <Locally Deleted>
	for _, c := range containers {
		var found = false
		containerJSON, err := client.Client.ContainerInspect(client.Context, c.ID)
		if err != nil {
			return URLList{}, err
		}
		containerJSONMap[c.ID] = containerJSON
		for internalPort, portbinding := range containerJSON.HostConfig.PortBindings {
			externalport, err := strconv.Atoi(portbinding[0].HostPort)
			if err != nil {
				return URLList{}, err
			}
			dockerURL := getMachineReadableFormatDocker(internalPort.Int(), externalport, portbinding[0].HostIP, containerJSON.Config.Labels[internalPort.Port()])
			for _, localurl := range localURLs {
				// only checks for Docker URLs
				if localurl.Kind != localConfigProvider.DOCKER {
					continue
				}
				if localurl.Port == dockerURL.Spec.Port && localurl.ExposedPort == dockerURL.Spec.ExternalPort {
					// URL is in both env file and Docker HostConfig
					dockerURL.Status.State = StateTypePushed
					urls = append(urls, dockerURL)
					found = true
					break
				}
			}
			if !found {
				// URL is in Docker HostConfig but not in env file
				dockerURL.Status.State = StateTypeLocallyDeleted
				urls = append(urls, dockerURL)
			}
		}
	}

	// iterating through URLs in local env.yaml
	// find out if there is a match config in Docker container
	// if the config does not exist in Docker container, then the URL is <Not Pushed>
	for _, localurl := range localURLs {
		// only checks for Docker URLs
		if localurl.Kind != localConfigProvider.DOCKER {
			continue
		}
		var found = false
		localURL := getMachineReadableFormatDocker(localurl.Port, localurl.ExposedPort, dockercomponent.LocalhostIP, localurl.Name)
		for _, c := range containers {
			containerJSON := containerJSONMap[c.ID]
			for internalPort, portbinding := range containerJSON.HostConfig.PortBindings {
				externalport, err := strconv.Atoi(portbinding[0].HostPort)
				if err != nil {
					return URLList{}, err
				}
				if localURL.Spec.Port == internalPort.Int() && localURL.Spec.ExternalPort == externalport {
					found = true
					break
				}
			}
		}
		if !found {
			// URL is in the env file but not pushed to Docker container
			localURL.Status.State = StateTypeNotPushed
			urls = append(urls, localURL)
		}
	}
	urlList := getMachineReadableFormatForList(urls)
	return urlList, nil
}

// GetProtocol returns the protocol string
func GetProtocol(route routev1.Route, ingress iextensionsv1.Ingress) string {
	if !reflect.DeepEqual(ingress, iextensionsv1.Ingress{}) && ingress.Spec.TLS != nil {
		return "https"
	} else if !reflect.DeepEqual(route, routev1.Route{}) && route.Spec.TLS != nil {
		return "https"
	}
	return "http"
}

// ConvertConfigURL converts ConfigURL to URL
func ConvertConfigURL(configURL localConfigProvider.LocalURL) URL {
	return URL{
		TypeMeta: metav1.TypeMeta{
			Kind:       "url",
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: configURL.Name,
		},
		Spec: URLSpec{
			Port:   configURL.Port,
			Secure: configURL.Secure,
			Kind:   localConfigProvider.ROUTE,
			Path:   "/",
		},
	}
}

// ConvertEnvinfoURL converts EnvinfoURL to URL
func ConvertEnvinfoURL(envinfoURL localConfigProvider.LocalURL, serviceName string) URL {
	hostString := fmt.Sprintf("%s.%s", envinfoURL.Name, envinfoURL.Host)
	// default to route kind if none is provided
	kind := envinfoURL.Kind
	if kind == "" {
		kind = localConfigProvider.ROUTE
	}
	url := URL{
		TypeMeta: metav1.TypeMeta{
			Kind:       "url",
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: envinfoURL.Name,
		},
		Spec: URLSpec{
			Port:   envinfoURL.Port,
			Secure: envinfoURL.Secure,
			Kind:   kind,
		},
	}
	if kind == localConfigProvider.INGRESS {
		url.Spec.Host = hostString
		if envinfoURL.Secure && len(envinfoURL.TLSSecret) > 0 {
			url.Spec.TLSSecret = envinfoURL.TLSSecret
		} else if envinfoURL.Secure {
			url.Spec.TLSSecret = fmt.Sprintf("%s-tlssecret", serviceName)
		}
	}
	return url
}

// GetURLString returns a string representation of given url
func GetURLString(protocol, URL, ingressDomain string, isS2I bool) string {
	if protocol == "" && URL == "" && ingressDomain == "" {
		return ""
	}
	if !isS2I && URL == "" {
		return protocol + "://" + ingressDomain
	}
	return protocol + "://" + URL
}

// Exists checks if the url exists in the component or not
// urlName is the name of the url for checking
// componentName is the name of the component to which the url's existence is checked
// applicationName is the name of the application to which the url's existence is checked
func Exists(client *occlient.Client, urlName string, componentName string, applicationName string) (bool, error) {
	urls, err := ListPushed(client, componentName, applicationName)
	if err != nil {
		return false, errors.Wrap(err, "unable to list the urls")
	}

	for _, url := range urls.Items {
		if url.Name == urlName {
			return true, nil
		}
	}
	return false, nil
}

// GetValidExposedPortNumber checks if the given exposed port number is a valid port or not
// if exposed port is not provided, a random free port will be generated and returned
func GetValidExposedPortNumber(exposedPort int) (int, error) {
	// exposed port number will be -1 if the user doesn't specify any port
	if exposedPort == -1 {
		freePort, err := util.HTTPGetFreePort()
		if err != nil {
			return -1, err
		}
		return freePort, nil
	} else {
		// check if the given port is available
		listener, err := net.Listen("tcp", ":"+strconv.Itoa(exposedPort))
		if err != nil {
			return -1, errors.Wrapf(err, "given port %d is not available, please choose another port", exposedPort)
		}
		defer listener.Close()
		return exposedPort, nil
	}
}

// getMachineReadableFormat gives machine readable URL definition
func getMachineReadableFormat(r routev1.Route) URL {
	return URL{
		TypeMeta:   metav1.TypeMeta{Kind: "url", APIVersion: apiVersion},
		ObjectMeta: metav1.ObjectMeta{Name: r.Labels[urlLabels.URLLabel]},
		Spec:       URLSpec{Host: r.Spec.Host, Port: r.Spec.Port.TargetPort.IntValue(), Protocol: GetProtocol(r, iextensionsv1.Ingress{}), Secure: r.Spec.TLS != nil, Path: r.Spec.Path, Kind: localConfigProvider.ROUTE},
	}

}

func getMachineReadableFormatForList(urls []URL) URLList {
	return URLList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "List",
			APIVersion: apiVersion,
		},
		ListMeta: metav1.ListMeta{},
		Items:    urls,
	}
}

func getMachineReadableFormatIngress(i iextensionsv1.Ingress) URL {
	url := URL{
		TypeMeta:   metav1.TypeMeta{Kind: "url", APIVersion: apiVersion},
		ObjectMeta: metav1.ObjectMeta{Name: i.Labels[urlLabels.URLLabel]},
		Spec:       URLSpec{Host: i.Spec.Rules[0].Host, Port: int(i.Spec.Rules[0].HTTP.Paths[0].Backend.ServicePort.IntVal), Secure: i.Spec.TLS != nil, Path: i.Spec.Rules[0].HTTP.Paths[0].Path, Kind: localConfigProvider.INGRESS},
	}
	if i.Spec.TLS != nil {
		url.Spec.TLSSecret = i.Spec.TLS[0].SecretName
	}
	return url

}

// ConvertIngressURLToIngress converts IngressURL to Ingress
func ConvertIngressURLToIngress(ingressURL URL, serviceName string) iextensionsv1.Ingress {
	port := intstr.IntOrString{
		Type:   intstr.Int,
		IntVal: int32(ingressURL.Spec.Port),
	}
	ingress := iextensionsv1.Ingress{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressURL.Name,
		},
		Spec: iextensionsv1.IngressSpec{
			Rules: []iextensionsv1.IngressRule{
				{
					Host: ingressURL.Spec.Host,
					IngressRuleValue: iextensionsv1.IngressRuleValue{
						HTTP: &iextensionsv1.HTTPIngressRuleValue{
							Paths: []iextensionsv1.HTTPIngressPath{
								{
									Path: ingressURL.Spec.Path,
									Backend: iextensionsv1.IngressBackend{
										ServiceName: serviceName,
										ServicePort: port,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if len(ingressURL.Spec.TLSSecret) > 0 {
		ingress.Spec.TLS = []iextensionsv1.IngressTLS{
			{
				Hosts: []string{
					ingressURL.Spec.Host,
				},
				SecretName: ingressURL.Spec.TLSSecret,
			},
		}
	}
	return ingress
}

func getMachineReadableFormatDocker(internalPort int, externalPort int, hostIP string, urlName string) URL {
	return URL{
		TypeMeta:   metav1.TypeMeta{Kind: "url", APIVersion: apiVersion},
		ObjectMeta: metav1.ObjectMeta{Name: urlName},
		Spec:       URLSpec{Host: hostIP, Port: internalPort, ExternalPort: externalPort},
	}
}

type PushParameters struct {
	ComponentName             string
	ApplicationName           string
	ConfigURLs                []localConfigProvider.LocalURL
	EnvURLS                   []localConfigProvider.LocalURL
	IsRouteSupported          bool
	IsExperimentalModeEnabled bool
	ContainerComponents       []devfilev1.Component
	IsS2I                     bool
}

// Push creates and deletes the required URLs
func Push(client *occlient.Client, kClient *kclient.Client, parameters PushParameters) error {
	urlLOCAL := make(map[string]URL)

	// in case the component is a s2i one
	// kClient will be nil
	if !parameters.IsS2I && kClient != nil {
		envURLMap := make(map[string]localConfigProvider.LocalURL)
		for _, url := range parameters.EnvURLS {
			if url.Kind == localConfigProvider.DOCKER {
				continue
			}
			envURLMap[url.Name] = url
		}
		for _, comp := range parameters.ContainerComponents {
			for _, endpoint := range comp.Container.Endpoints {
				// skip URL creation if the URL is not publicly exposed
				if endpoint.Exposure == devfilev1.NoneEndpointExposure || endpoint.Exposure == devfilev1.InternalEndpointExposure {
					continue
				}
				secure := false
				if endpoint.Secure || endpoint.Protocol == "https" || endpoint.Protocol == "wss" {
					secure = true
				}
				path := "/"
				if endpoint.Path != "" {
					path = endpoint.Path
				}
				name := getValidURLName(endpoint.Name)
				existInEnv := false
				if url, exist := envURLMap[endpoint.Name]; exist {
					existInEnv = true
					urlLOCAL[name] = URL{
						Spec: URLSpec{
							Host:      url.Host,
							Port:      int(endpoint.TargetPort),
							Secure:    secure,
							TLSSecret: url.TLSSecret,
							Kind:      url.Kind,
							Path:      path,
						},
					}
				}
				if !existInEnv {
					if !parameters.IsRouteSupported {
						// display warning since Host info is missing
						log.Warningf("Unable to create ingress, missing host information for Endpoint %v, please check instructions on URL creation (refer `odo url create --help`)\n", endpoint.Name)
					} else {
						urlLOCAL[name] = URL{
							Spec: URLSpec{
								Port:   int(endpoint.TargetPort),
								Secure: secure,
								Kind:   localConfigProvider.ROUTE,
								Path:   path,
							},
						}
					}
				}
			}
		}
	} else {
		urls := parameters.ConfigURLs
		for _, url := range urls {
			urlLOCAL[url.Name] = URL{
				Spec: URLSpec{
					Port:   url.Port,
					Secure: url.Secure,
					Kind:   localConfigProvider.ROUTE,
					Path:   "/",
				},
			}
		}
	}

	log.Info("\nApplying URL changes")

	urlCLUSTER := make(map[string]URL)
	if !parameters.IsS2I && kClient != nil {
		urlList, err := ListPushedIngress(kClient, parameters.ComponentName)
		if err != nil {
			return err
		}
		for _, url := range urlList.Items {
			urlCLUSTER[url.Name] = URL{
				Spec: URLSpec{
					Host:   url.Spec.Host,
					Port:   url.Spec.Port,
					Kind:   localConfigProvider.INGRESS,
					Secure: url.Spec.Secure,
					Path:   url.Spec.Path,
				},
			}
		}
	}

	if parameters.IsRouteSupported {
		urlPushedRoutes, err := ListPushed(client, parameters.ComponentName, parameters.ApplicationName)
		if err != nil {
			return err
		}
		for _, urlRoute := range urlPushedRoutes.Items {
			urlCLUSTER[urlRoute.Name] = URL{
				Spec: URLSpec{
					Port:   urlRoute.Spec.Port,
					Kind:   localConfigProvider.ROUTE,
					Secure: urlRoute.Spec.Secure,
					Path:   urlRoute.Spec.Path,
				},
			}
		}
	}

	urlChange := false

	// find URLs to delete
	for urlName, urlSpec := range urlCLUSTER {
		val, ok := urlLOCAL[urlName]

		configMismatch := false
		if ok {
			// since the host stored in an ingress
			// is the combination of name and host of the url
			if val.Spec.Kind == localConfigProvider.INGRESS {
				val.Spec.Host = fmt.Sprintf("%v.%v", urlName, val.Spec.Host)
			}
			if !reflect.DeepEqual(val.Spec, urlSpec.Spec) {
				configMismatch = true
				klog.V(4).Infof("config and cluster mismatch for url %s", urlName)
			}
		}

		if !ok || configMismatch {
			if urlSpec.Spec.Kind == localConfigProvider.INGRESS && kClient == nil {
				continue
			}
			// delete the url
			deleteURLName := urlName
			if !parameters.IsS2I && kClient != nil {
				// route/ingress name is defined as <urlName>-<componentName>
				// to avoid error due to duplicate ingress name defined in different devfile components
				deleteURLName = fmt.Sprintf("%s-%s", urlName, parameters.ComponentName)
			}
			err := Delete(client, kClient, deleteURLName, parameters.ApplicationName, urlSpec.Spec.Kind, parameters.IsS2I)
			if err != nil {
				return err
			}
			log.Successf("URL %s successfully deleted", urlName)
			urlChange = true
			delete(urlCLUSTER, urlName)
			continue
		}
	}

	// find URLs to create
	for urlName, urlInfo := range urlLOCAL {
		_, ok := urlCLUSTER[urlName]
		if !ok {
			if urlInfo.Spec.Kind == localConfigProvider.INGRESS && kClient == nil {
				continue
			}
			createParameters := CreateParameters{
				urlName:         urlName,
				portNumber:      urlInfo.Spec.Port,
				secureURL:       urlInfo.Spec.Secure,
				componentName:   parameters.ComponentName,
				applicationName: parameters.ApplicationName,
				host:            urlInfo.Spec.Host,
				secretName:      urlInfo.Spec.TLSSecret,
				urlKind:         urlInfo.Spec.Kind,
				path:            urlInfo.Spec.Path,
			}
			host, err := Create(client, kClient, createParameters, parameters.IsRouteSupported, parameters.IsS2I)
			if err != nil {
				return err
			}
			log.Successf("URL %s: %s%s created", urlName, host, urlInfo.Spec.Path)
			urlChange = true
		}
	}

	if !urlChange {
		log.Success("URLs are synced with the cluster, no changes are required.")
	}

	return nil
}

// getValidURLName returns valid URL resource name for Kubernetes based cluster
func getValidURLName(name string) string {
	trimmedName := strings.TrimSpace(slug.Make(name))
	trimmedName = util.TruncateString(trimmedName, 15)
	return trimmedName
}
