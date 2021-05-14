package broadcaster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"

	configv1alpha1 "github.com/liqotech/liqo/apis/config/v1alpha1"
	discoveryv1alpha1 "github.com/liqotech/liqo/apis/discovery/v1alpha1"
	advtypes "github.com/liqotech/liqo/apis/sharing/v1alpha1"
	liqoconst "github.com/liqotech/liqo/pkg/consts"
	crdclient "github.com/liqotech/liqo/pkg/crdClient"
	"github.com/liqotech/liqo/pkg/kubeconfig"
	"github.com/liqotech/liqo/pkg/utils"
	pkg "github.com/liqotech/liqo/pkg/virtualKubelet"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

// AdvertisementBroadcaster models data and structures needed by a broadcaster instance.
type AdvertisementBroadcaster struct {
	// local-related variables
	LocalClient     *crdclient.CRDClient
	DiscoveryClient *crdclient.CRDClient
	// remote-related variables
	KubeconfigSecretForForeign *corev1.Secret       // secret containing the kubeconfig that will be sent to the foreign cluster
	RemoteClient               *crdclient.CRDClient // client to create Advertisements and Secrets on the foreign cluster
	// configuration variables
	HomeClusterID      string
	ForeignClusterID   string
	PeeringRequestName string
	ClusterConfig      configv1alpha1.ClusterConfigSpec
	mutex              sync.Mutex
}

// AdvResources contains all resources to be returned in an advertisement.
type AdvResources struct {
	PhysicalNodes *corev1.NodeList
	VirtualNodes  *corev1.NodeList
	Availability  corev1.ResourceList
	Limits        corev1.ResourceList
	Images        []corev1.ContainerImage
	Labels        map[string]string
}

type apiConfigProviderEnv struct{}

func (p *apiConfigProviderEnv) GetAPIServerConfig() *configv1alpha1.APIServerConfig {
	return &configv1alpha1.APIServerConfig{
		Address:   os.Getenv("APISERVER"),
		Port:      os.Getenv("APISERVER_PORT"),
		TrustedCA: os.Getenv("APISERVER_TRUSTED") == "true",
	}
}

// StartBroadcaster start the broadcaster which sends Advertisement messages
// it reads the Secret to get the kubeconfig to the remote cluster and create a client for it
// parameters
// - homeClusterId: the cluster ID of your cluster (must be a UUID)
// - localKubeconfigPath: the path to the kubeconfig of the local cluster. Set it only when you are debugging and need
//   to launch the program as a process and not inside Kubernetes
// - peeringRequestName: the name of the PeeringRequest containing the reference to the secret with the kubeconfig for
//   creating Advertisements CR on foreign cluster
// - saName: The name of the ServiceAccount used to create the kubeconfig that will be sent to the foreign cluster with
//   the permissions to create resources on local cluster.
func StartBroadcaster(homeClusterID, localKubeconfigPath, peeringRequestName, saName string) error {
	klog.V(6).Info("starting broadcaster")

	// create the Advertisement client to the local cluster
	localClient, err := advtypes.CreateAdvertisementClient(localKubeconfigPath, nil, true, nil)
	if err != nil {
		klog.Errorln(err, "Unable to create client to local cluster")
		return err
	}

	// create the discovery client
	config, err := crdclient.NewKubeconfig(localKubeconfigPath, &discoveryv1alpha1.GroupVersion, nil)
	if err != nil {
		klog.Error(err, err.Error())
		return err
	}
	discoveryClient, err := crdclient.NewFromConfig(config)
	if err != nil {
		klog.Error(err, err.Error())
		return err
	}

	// get the PeeringRequest from the foreign cluster which requested resources
	tmp, err := discoveryClient.Resource("peeringrequests").Get(peeringRequestName, &metav1.GetOptions{})
	if err != nil {
		klog.Errorln(err, "Unable to get PeeringRequest "+peeringRequestName)
		return err
	}
	pr, ok := tmp.(*discoveryv1alpha1.PeeringRequest)
	if !ok {
		return errors.New("retrieved object is not a PeeringRequest")
	}

	foreignClusterId := pr.Name

	// get the Secret with the permission to create Advertisements and Secrets on foreign cluster
	secretForAdvertisementCreation, err := localClient.Client().CoreV1().Secrets(pr.Spec.KubeConfigRef.Namespace).
		Get(context.TODO(), pr.Spec.KubeConfigRef.Name, metav1.GetOptions{})
	if err != nil {
		klog.Errorln(err, "Unable to get PeeringRequest secret")
		return err
	}

	// create the Advertisement client to the remote cluster, using the retrieved Secret
	var remoteClient *crdclient.CRDClient
	var retry int

	// create a CRD-client to the foreign cluster
	for retry = 0; retry < 3; retry++ {
		remoteClient, err = advtypes.CreateAdvertisementClient("", secretForAdvertisementCreation, true, nil)
		if err != nil {
			klog.Errorln(err, "Unable to create client to remote cluster "+foreignClusterId+". Retry in 1 minute")
			time.Sleep(1 * time.Minute)
		} else {
			break
		}
	}
	if retry == 3 {
		klog.Errorln(err, "Failed to create client to remote cluster "+foreignClusterId)
		return err
	}

	klog.Info("Correctly created client to remote cluster " + foreignClusterId)
	broadcaster := AdvertisementBroadcaster{
		LocalClient:        localClient,
		DiscoveryClient:    discoveryClient,
		RemoteClient:       remoteClient,
		HomeClusterID:      homeClusterID,
		ForeignClusterID:   pr.Name,
		PeeringRequestName: peeringRequestName,
	}

	kubeconfigSecretName := pkg.VirtualKubeletSecPrefix + homeClusterID

	// create the kubeconfig to allow the foreign cluster to create resources on local cluster
	kubeconfigForForeignCluster, err := kubeconfig.CreateKubeConfig(&apiConfigProviderEnv{}, localClient.Client(), saName, pr.Spec.Namespace)
	if err != nil {
		klog.Errorln(err, "Unable to create Kubeconfig")
		return err
	}
	// put the kubeconfig in a Secret, which is created on the foreign cluster
	kubeconfigSecretForForeign := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeconfigSecretName,
			Namespace: pr.Spec.Namespace,
		},
		Data: nil,
		StringData: map[string]string{
			"kubeconfig": kubeconfigForForeignCluster,
		},
	}
	broadcaster.KubeconfigSecretForForeign = kubeconfigSecretForForeign
	_, err = broadcaster.SendSecretToForeignCluster(kubeconfigSecretForForeign)
	if err != nil {
		// secret not created, without it the vk cannot be launched: just log and exit
		klog.Errorf("Unable to create secret for virtualKubelet on remote cluster %v; error: %v", foreignClusterId, err)
		return err
	}
	// secret correctly created on foreign cluster, now launch the broadcaster to create Advertisement

	broadcaster.WatchConfiguration(localKubeconfigPath, nil)

	broadcaster.GenerateAdvertisement()
	// if we come here there has been an error while the broadcaster was running
	return errors.New("error while running Advertisement Broadcaster")
}

// GenerateAdvertisement generates an Advertisement message every 10 minutes and post it to remote clusters.
func (b *AdvertisementBroadcaster) GenerateAdvertisement() {
	var once sync.Once

	for {
		_, err := b.SendSecretToForeignCluster(b.KubeconfigSecretForForeign)
		if err != nil {
			klog.Errorln(err, "Error while sending Secret for virtual-kubelet to cluster "+b.ForeignClusterID)
			time.Sleep(1 * time.Minute)
			continue
		}

		advRes, err := b.GetResourcesForAdv()
		if err != nil {
			klog.Errorln(err, "Error while computing resources for Advertisement")
			time.Sleep(1 * time.Minute)
			continue
		}

		// create the Advertisement on the foreign cluster
		advToCreate := b.CreateAdvertisement(advRes)
		adv, err := b.SendAdvertisementToForeignCluster(advToCreate)
		if err != nil {
			klog.Errorln(err, "Error while sending Advertisement to cluster "+b.ForeignClusterID)
			time.Sleep(1 * time.Minute)
			continue
		}

		// start the remote watcher over this Advertisement; the watcher must be launched only once
		go once.Do(func() {
			b.WatchAdvertisement(adv.Name)
		})

		time.Sleep(10 * time.Minute)
	}
}

// CreateAdvertisement creates advertisement message.
func (b *AdvertisementBroadcaster) CreateAdvertisement(advRes *AdvResources) advtypes.Advertisement {
	// set prices field
	prices := ComputePrices(advRes.Images)
	// use virtual nodes to build neighbors
	neighbors := make(map[corev1.ResourceName]corev1.ResourceList)
	for _, vnode := range advRes.VirtualNodes.Items {
		neighbors[corev1.ResourceName(vnode.Name)] = vnode.Status.Allocatable
	}

	adv := advtypes.Advertisement{
		ObjectMeta: metav1.ObjectMeta{
			Name: pkg.AdvertisementPrefix + b.HomeClusterID,
		},
		Spec: advtypes.AdvertisementSpec{
			ClusterId: b.HomeClusterID,
			Images:    advRes.Images,
			LimitRange: corev1.LimitRangeSpec{
				Limits: []corev1.LimitRangeItem{
					{
						Type:                 "",
						Max:                  advRes.Limits,
						Min:                  nil,
						Default:              nil,
						DefaultRequest:       nil,
						MaxLimitRequestRatio: nil,
					},
				},
			},
			ResourceQuota: corev1.ResourceQuotaSpec{
				Hard:          advRes.Availability,
				Scopes:        nil,
				ScopeSelector: nil,
			},
			Labels:     advRes.Labels,
			Neighbors:  neighbors,
			Properties: nil,
			Prices:     prices,
			KubeConfigRef: corev1.SecretReference{
				Namespace: b.KubeconfigSecretForForeign.Namespace,
				Name:      b.KubeconfigSecretForForeign.Name,
			},
			Timestamp:  metav1.NewTime(time.Now()),
			TimeToLive: metav1.NewTime(time.Now().Add(30 * time.Minute)),
		},
	}
	return adv
}

func (b *AdvertisementBroadcaster) GetResourcesForAdv() (advRes *AdvResources, err error) {
	// get physical and virtual nodes in the cluster
	physicalNodes, err := b.LocalClient.Client().CoreV1().Nodes().List(context.TODO(),
		metav1.ListOptions{LabelSelector: fmt.Sprintf("%v != %v", liqoconst.TypeLabel, liqoconst.TypeNode)})
	if err != nil {
		klog.Errorln("Could not get physical nodes, retry in 1 minute")
		return nil, err
	}
	virtualNodes, err := b.LocalClient.Client().CoreV1().Nodes().List(context.TODO(),
		metav1.ListOptions{LabelSelector: fmt.Sprintf("%v = %v", liqoconst.TypeLabel, liqoconst.TypeNode)})
	if err != nil {
		klog.Errorln("Could not get virtual nodes, retry in 1 minute")
		return nil, err
	}
	// get resources used by pods in the cluster
	fieldSelector, err := fields.ParseSelector("status.phase!=" + string(corev1.PodSucceeded) + ",status.phase!=" + string(corev1.PodFailed))
	if err != nil {
		return nil, err
	}
	nodeNonTerminatedPodsList, err := b.LocalClient.Client().CoreV1().Pods("").List(context.TODO(),
		metav1.ListOptions{FieldSelector: fieldSelector.String()})
	if err != nil {
		klog.Errorln("Could not list pods, retry in 1 minute")
		return nil, err
	}
	reqs, limits := GetAllPodsResources(nodeNonTerminatedPodsList)
	// compute resources to be announced to the other cluster
	availability, images := ComputeAnnouncedResources(physicalNodes, reqs,
		int64(b.ClusterConfig.AdvertisementConfig.OutgoingConfig.ResourceSharingPercentage))

	labels := GetLabels(physicalNodes, b.ClusterConfig.AdvertisementConfig.LabelPolicies)

	return &AdvResources{
		PhysicalNodes: physicalNodes,
		VirtualNodes:  virtualNodes,
		Availability:  availability,
		Limits:        limits,
		Images:        images,
		Labels:        labels,
	}, nil
}

func (b *AdvertisementBroadcaster) SendAdvertisementToForeignCluster(advToCreate advtypes.Advertisement) (*advtypes.Advertisement, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	var adv *advtypes.Advertisement

	// try to get the Advertisement on remote cluster
	obj, err := b.RemoteClient.Resource("advertisements").Get(advToCreate.Name, &metav1.GetOptions{})
	if err == nil {
		// Advertisement already created, update it
		adv = obj.(*advtypes.Advertisement)
		advToCreate.ObjectMeta = adv.ObjectMeta
		_, err = b.RemoteClient.Resource("advertisements").Update(adv.Name, &advToCreate, &metav1.UpdateOptions{})
		if err != nil {
			klog.Errorln("Unable to update Advertisement " + advToCreate.Name)
			return nil, err
		}
	} else if k8serrors.IsNotFound(err) {
		secretForeign, err := b.RemoteClient.Client().CoreV1().Secrets(b.KubeconfigSecretForForeign.Namespace).Get(context.TODO(),
			b.KubeconfigSecretForForeign.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		// Advertisement not found, create it
		obj, err := b.RemoteClient.Resource("advertisements").Create(&advToCreate, &metav1.CreateOptions{})
		if err != nil {
			klog.Errorln("Unable to create Advertisement " + advToCreate.Name + " on remote cluster " + b.ForeignClusterID)
			return nil, err
		} else {
			// Advertisement created, set the owner reference of the secret so that it is deleted when the adv is removed
			adv = obj.(*advtypes.Advertisement)
			klog.Info("Correctly created advertisement on remote cluster " + b.ForeignClusterID)
			adv.Kind = "Advertisement"
			adv.APIVersion = advtypes.GroupVersion.String()

			secretForeign.SetOwnerReferences(utils.GetOwnerReference(adv))
			_, err = b.RemoteClient.Client().CoreV1().Secrets(secretForeign.Namespace).Update(context.TODO(), secretForeign, metav1.UpdateOptions{})
			if err != nil {
				klog.Errorln(err, "Unable to update secret "+b.KubeconfigSecretForForeign.Name)
			}
		}
	} else {
		klog.Errorln("Unexpected error while getting Advertisement " + advToCreate.Name)
		return nil, err
	}
	return adv, nil
}

func (b *AdvertisementBroadcaster) SendSecretToForeignCluster(secret *corev1.Secret) (*corev1.Secret, error) {
	secretForeign, err := b.RemoteClient.Client().CoreV1().Secrets(secret.Namespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
	if err == nil {
		// secret already created, update it
		secret.SetResourceVersion(secretForeign.ResourceVersion)
		secret.SetUID(secretForeign.UID)
		secretForeign, err = b.RemoteClient.Client().CoreV1().Secrets(secret.Namespace).Update(context.TODO(), secret, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Unable to update secret %v on remote cluster %v; error: %v", secret.Name, b.ForeignClusterID, err)
			return nil, err
		}
		klog.Infof("Correctly updated secret %v on remote cluster %v", secret.Name, b.ForeignClusterID)
	} else if k8serrors.IsNotFound(err) {
		// secret not found, create it
		secret.SetResourceVersion("")
		secret.SetUID("")
		secretForeign, err = b.RemoteClient.Client().CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		if err != nil {
			// secret not created, without it the vk cannot be launched: just log and exit
			klog.Errorf("Unable to create secret %v on remote cluster %v; error: %v", secret.Name, b.ForeignClusterID, err)
			return nil, err
		}
		klog.Infof("Correctly created secret %v on remote cluster %v", secret.Name, b.ForeignClusterID)
	} else {
		klog.Errorln("Unexpected error while getting Secret " + secret.Name)
		return nil, err
	}
	return secretForeign, nil
}

func (b *AdvertisementBroadcaster) NotifyAdvertisementDeletion() error {
	advName := pkg.AdvertisementPrefix + b.HomeClusterID
	// delete adv to inform the vk to do the cleanup
	err := b.RemoteClient.Resource("advertisements").Delete(advName, &metav1.DeleteOptions{})
	if err != nil {
		klog.Error("Unable to delete Advertisement " + advName)
		return err
	}
	return nil
}

// GetAllPodsResources get resources used by pods on physical nodes.
func GetAllPodsResources(nodeNonTerminatedPodsList *corev1.PodList) (requests corev1.ResourceList, limits corev1.ResourceList) {
	// remove pods on virtual nodes
	for i, pod := range nodeNonTerminatedPodsList.Items {
		if strings.HasPrefix(pod.Spec.NodeName, "liqo-") {
			nodeNonTerminatedPodsList.Items[i] = corev1.Pod{}
		} else if pod.Labels != nil {
			if _, ok := pod.Labels[forge.LiqoOutgoingKey]; ok {
				// TODO: is this pod offloaded by the cluster where we will send this advertisement?
				nodeNonTerminatedPodsList.Items[i] = corev1.Pod{}
			}
		}
	}
	requests, limits = getPodsTotalRequestsAndLimits(nodeNonTerminatedPodsList)
	return requests, limits
}

func getPodsTotalRequestsAndLimits(podList *corev1.PodList) (reqs map[corev1.ResourceName]resource.Quantity, limits map[corev1.ResourceName]resource.Quantity) {
	reqs, limits = map[corev1.ResourceName]resource.Quantity{}, map[corev1.ResourceName]resource.Quantity{}
	for i := range podList.Items {
		pod := podList.Items[i]
		podReqs, podLimits := resourcehelper.PodRequestsAndLimits(&pod)
		for podReqName, podReqValue := range podReqs {
			if value, ok := reqs[podReqName]; !ok {
				reqs[podReqName] = podReqValue.DeepCopy()
			} else {
				value.Add(podReqValue)
				reqs[podReqName] = value
			}
		}
		for podLimitName, podLimitValue := range podLimits {
			if value, ok := limits[podLimitName]; !ok {
				limits[podLimitName] = podLimitValue.DeepCopy()
			} else {
				value.Add(podLimitValue)
				limits[podLimitName] = value
			}
		}
	}
	return
}

// GetClusterResources get cluster resources (cpu, ram, pods, ...) and images.
func GetClusterResources(nodes []corev1.Node) (corev1.ResourceList, []corev1.ContainerImage) {
	clusterImages := make([]corev1.ContainerImage, 0)

	availability := corev1.ResourceList{}
	for _, node := range nodes {
		addResourceLists(&availability, &node.Status.Allocatable)

		nodeImages := GetNodeImages(node)
		clusterImages = append(clusterImages, nodeImages...)
	}
	return availability, clusterImages
}

func addResourceLists(dst *corev1.ResourceList, toAdd *corev1.ResourceList) {
	for k, v := range *toAdd {
		qnt, ok := (*dst)[k]
		if ok {
			// value already exists, add to it
			qnt.Add(v)
			(*dst)[k] = qnt
		} else {
			// value does not exists, create it
			(*dst)[k] = v.DeepCopy()
		}
	}
}
