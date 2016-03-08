// Package app does all of the work necessary to create
// APIServer by binding together the API, master and APIServer infrastructure.
package app

import (
	"github.com/gosharplite/sketch/cmd/skt-apiserver/app/options"
)

// Run runs the specified APIServer.  This should never exit.
func Run(s *options.APIServer) error {
	verifyClusterIPFlags(s)

	// If advertise-address is not specified, use bind-address. If bind-address
	// is not usable (unset, 0.0.0.0, or loopback), we will use the host's default
	// interface as valid public addr for master (see: util/net#ValidPublicAddrForMaster)
	if s.AdvertiseAddress == nil || s.AdvertiseAddress.IsUnspecified() {
		hostIP, err := utilnet.ChooseBindAddress(s.BindAddress)
		if err != nil {
			glog.Fatalf("Unable to find suitable network address.error='%v' . "+
				"Try to set the AdvertiseAddress directly or provide a valid BindAddress to fix this.", err)
		}
		s.AdvertiseAddress = hostIP
	}
	glog.Infof("Will report %v as public IP address.", s.AdvertiseAddress)

	if len(s.EtcdServerList) == 0 {
		glog.Fatalf("--etcd-servers must be specified")
	}

	if s.KubernetesServiceNodePort > 0 && !s.ServiceNodePortRange.Contains(s.KubernetesServiceNodePort) {
		glog.Fatalf("Kubernetes service port range %v doesn't contain %v", s.ServiceNodePortRange, (s.KubernetesServiceNodePort))
	}

	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: s.AllowPrivileged,
		// TODO(vmarmol): Implement support for HostNetworkSources.
		PrivilegedSources: capabilities.PrivilegedSources{
			HostNetworkSources: []string{},
			HostPIDSources:     []string{},
			HostIPCSources:     []string{},
		},
		PerConnectionBandwidthLimitBytesPerSec: s.MaxConnectionBytesPerSec,
	})

	cloud, err := cloudprovider.InitCloudProvider(s.CloudProvider, s.CloudConfigFile)
	if err != nil {
		glog.Fatalf("Cloud provider could not be initialized: %v", err)
	}

	// Setup tunneler if needed
	var tunneler master.Tunneler
	var proxyDialerFn apiserver.ProxyDialerFunc
	if len(s.SSHUser) > 0 {
		// Get ssh key distribution func, if supported
		var installSSH master.InstallSSHKey
		if cloud != nil {
			if instances, supported := cloud.Instances(); supported {
				installSSH = instances.AddSSHKeyToAllInstances
			}
		}
		if s.KubeletConfig.Port == 0 {
			glog.Fatalf("Must enable kubelet port if proxy ssh-tunneling is specified.")
		}
		// Set up the tunneler
		// TODO(cjcullen): If we want this to handle per-kubelet ports or other
		// kubelet listen-addresses, we need to plumb through options.
		healthCheckPath := &url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(s.KubeletConfig.Port), 10)),
			Path:   "healthz",
		}
		tunneler = master.NewSSHTunneler(s.SSHUser, s.SSHKeyfile, healthCheckPath, installSSH)

		// Use the tunneler's dialer to connect to the kubelet
		s.KubeletConfig.Dial = tunneler.Dial
		// Use the tunneler's dialer when proxying to pods, services, and nodes
		proxyDialerFn = tunneler.Dial
	}

	// Proxying to pods and services is IP-based... don't expect to be able to verify the hostname
	proxyTLSClientConfig := &tls.Config{InsecureSkipVerify: true}

	kubeletClient, err := kubeletclient.NewStaticKubeletClient(&s.KubeletConfig)
	if err != nil {
		glog.Fatalf("Failure to start kubelet client: %v", err)
	}

	apiGroupVersionOverrides, err := parseRuntimeConfig(s)
	if err != nil {
		glog.Fatalf("error in parsing runtime-config: %s", err)
	}

	clientConfig := &restclient.Config{
		Host: net.JoinHostPort(s.InsecureBindAddress.String(), strconv.Itoa(s.InsecurePort)),
		// Increase QPS limits. The client is currently passed to all admission plugins,
		// and those can be throttled in case of higher load on apiserver - see #22340 and #22422
		// for more details. Once #22422 is fixed, we may want to remove it.
		QPS:   50,
		Burst: 100,
	}
	if len(s.DeprecatedStorageVersion) != 0 {
		gv, err := unversioned.ParseGroupVersion(s.DeprecatedStorageVersion)
		if err != nil {
			glog.Fatalf("error in parsing group version: %s", err)
		}
		clientConfig.GroupVersion = &gv
	}

	client, err := clientset.NewForConfig(clientConfig)
	if err != nil {
		glog.Errorf("Failed to create clientset: %v", err)
	}

	legacyV1Group, err := registered.Group(api.GroupName)
	if err != nil {
		return err
	}

	storageDestinations := genericapiserver.NewStorageDestinations()

	storageVersions := s.StorageGroupsToGroupVersions()
	if _, found := storageVersions[legacyV1Group.GroupVersion.Group]; !found {
		glog.Fatalf("Couldn't find the storage version for group: %q in storageVersions: %v", legacyV1Group.GroupVersion.Group, storageVersions)
	}
	etcdStorage, err := newEtcd(s.EtcdServerList, api.Codecs, storageVersions[legacyV1Group.GroupVersion.Group], "/__internal", s.EtcdPathPrefix, s.EtcdQuorumRead)
	if err != nil {
		glog.Fatalf("Invalid storage version or misconfigured etcd: %v", err)
	}
	storageDestinations.AddAPIGroup("", etcdStorage)

	if !apiGroupVersionOverrides["extensions/v1beta1"].Disable {
		glog.Infof("Configuring extensions/v1beta1 storage destination")
		expGroup, err := registered.Group(extensions.GroupName)
		if err != nil {
			glog.Fatalf("Extensions API is enabled in runtime config, but not enabled in the environment variable KUBE_API_VERSIONS. Error: %v", err)
		}
		if _, found := storageVersions[expGroup.GroupVersion.Group]; !found {
			glog.Fatalf("Couldn't find the storage version for group: %q in storageVersions: %v", expGroup.GroupVersion.Group, storageVersions)
		}
		expEtcdStorage, err := newEtcd(s.EtcdServerList, api.Codecs, storageVersions[expGroup.GroupVersion.Group], "extensions/__internal", s.EtcdPathPrefix, s.EtcdQuorumRead)
		if err != nil {
			glog.Fatalf("Invalid extensions storage version or misconfigured etcd: %v", err)
		}
		storageDestinations.AddAPIGroup(extensions.GroupName, expEtcdStorage)

		// Since HPA has been moved to the autoscaling group, we need to make
		// sure autoscaling has a storage destination. If the autoscaling group
		// itself is on, it will overwrite this decision below.
		storageDestinations.AddAPIGroup(autoscaling.GroupName, expEtcdStorage)

		// Since Job has been moved to the batch group, we need to make
		// sure batch has a storage destination. If the batch group
		// itself is on, it will overwrite this decision below.
		storageDestinations.AddAPIGroup(batch.GroupName, expEtcdStorage)
	}

	// autoscaling/v1/horizontalpodautoscalers is a move from extensions/v1beta1/horizontalpodautoscalers.
	// The storage version needs to be either extensions/v1beta1 or autoscaling/v1.
	// Users must roll forward while using 1.2, because we will require the latter for 1.3.
	if !apiGroupVersionOverrides["autoscaling/v1"].Disable {
		glog.Infof("Configuring autoscaling/v1 storage destination")
		autoscalingGroup, err := registered.Group(autoscaling.GroupName)
		if err != nil {
			glog.Fatalf("Autoscaling API is enabled in runtime config, but not enabled in the environment variable KUBE_API_VERSIONS. Error: %v", err)
		}
		// Figure out what storage group/version we should use.
		storageGroupVersion, found := storageVersions[autoscalingGroup.GroupVersion.Group]
		if !found {
			glog.Fatalf("Couldn't find the storage version for group: %q in storageVersions: %v", autoscalingGroup.GroupVersion.Group, storageVersions)
		}

		if storageGroupVersion != "autoscaling/v1" && storageGroupVersion != "extensions/v1beta1" {
			glog.Fatalf("The storage version for autoscaling must be either 'autoscaling/v1' or 'extensions/v1beta1'")
		}
		glog.Infof("Using %v for autoscaling group storage version", storageGroupVersion)
		autoscalingEtcdStorage, err := newEtcd(s.EtcdServerList, api.Codecs, storageGroupVersion, "extensions/__internal", s.EtcdPathPrefix, s.EtcdQuorumRead)
		if err != nil {
			glog.Fatalf("Invalid extensions storage version or misconfigured etcd: %v", err)
		}
		storageDestinations.AddAPIGroup(autoscaling.GroupName, autoscalingEtcdStorage)
	}

	// batch/v1/job is a move from extensions/v1beta1/job. The storage
	// version needs to be either extensions/v1beta1 or batch/v1. Users
	// must roll forward while using 1.2, because we will require the
	// latter for 1.3.
	if !apiGroupVersionOverrides["batch/v1"].Disable {
		glog.Infof("Configuring batch/v1 storage destination")
		batchGroup, err := registered.Group(batch.GroupName)
		if err != nil {
			glog.Fatalf("Batch API is enabled in runtime config, but not enabled in the environment variable KUBE_API_VERSIONS. Error: %v", err)
		}
		// Figure out what storage group/version we should use.
		storageGroupVersion, found := storageVersions[batchGroup.GroupVersion.Group]
		if !found {
			glog.Fatalf("Couldn't find the storage version for group: %q in storageVersions: %v", batchGroup.GroupVersion.Group, storageVersions)
		}

		if storageGroupVersion != "batch/v1" && storageGroupVersion != "extensions/v1beta1" {
			glog.Fatalf("The storage version for batch must be either 'batch/v1' or 'extensions/v1beta1'")
		}
		glog.Infof("Using %v for batch group storage version", storageGroupVersion)
		batchEtcdStorage, err := newEtcd(s.EtcdServerList, api.Codecs, storageGroupVersion, "extensions/__internal", s.EtcdPathPrefix, s.EtcdQuorumRead)
		if err != nil {
			glog.Fatalf("Invalid extensions storage version or misconfigured etcd: %v", err)
		}
		storageDestinations.AddAPIGroup(batch.GroupName, batchEtcdStorage)
	}

	updateEtcdOverrides(s.EtcdServersOverrides, storageVersions, s.EtcdPathPrefix, s.EtcdQuorumRead, &storageDestinations, newEtcd)

	n := s.ServiceClusterIPRange

	// Default to the private server key for service account token signing
	if s.ServiceAccountKeyFile == "" && s.TLSPrivateKeyFile != "" {
		if authenticator.IsValidServiceAccountKeyFile(s.TLSPrivateKeyFile) {
			s.ServiceAccountKeyFile = s.TLSPrivateKeyFile
		} else {
			glog.Warning("No RSA key provided, service account token authentication disabled")
		}
	}

	var serviceAccountGetter serviceaccount.ServiceAccountTokenGetter
	if s.ServiceAccountLookup {
		// If we need to look up service accounts and tokens,
		// go directly to etcd to avoid recursive auth insanity
		serviceAccountGetter = serviceaccountcontroller.NewGetterFromStorageInterface(etcdStorage)
	}

	authenticator, err := authenticator.New(authenticator.AuthenticatorConfig{
		BasicAuthFile:             s.BasicAuthFile,
		ClientCAFile:              s.ClientCAFile,
		TokenAuthFile:             s.TokenAuthFile,
		OIDCIssuerURL:             s.OIDCIssuerURL,
		OIDCClientID:              s.OIDCClientID,
		OIDCCAFile:                s.OIDCCAFile,
		OIDCUsernameClaim:         s.OIDCUsernameClaim,
		OIDCGroupsClaim:           s.OIDCGroupsClaim,
		ServiceAccountKeyFile:     s.ServiceAccountKeyFile,
		ServiceAccountLookup:      s.ServiceAccountLookup,
		ServiceAccountTokenGetter: serviceAccountGetter,
		KeystoneURL:               s.KeystoneURL,
	})

	if err != nil {
		glog.Fatalf("Invalid Authentication Config: %v", err)
	}

	authorizationModeNames := strings.Split(s.AuthorizationMode, ",")
	authorizer, err := apiserver.NewAuthorizerFromAuthorizationConfig(authorizationModeNames, s.AuthorizationConfig)
	if err != nil {
		glog.Fatalf("Invalid Authorization Config: %v", err)
	}

	admissionControlPluginNames := strings.Split(s.AdmissionControl, ",")
	admissionController := admission.NewFromPlugins(client, admissionControlPluginNames, s.AdmissionControlConfigFile)

	if len(s.ExternalHost) == 0 {
		// TODO: extend for other providers
		if s.CloudProvider == "gce" {
			instances, supported := cloud.Instances()
			if !supported {
				glog.Fatalf("GCE cloud provider has no instances.  this shouldn't happen. exiting.")
			}
			name, err := os.Hostname()
			if err != nil {
				glog.Fatalf("Failed to get hostname: %v", err)
			}
			addrs, err := instances.NodeAddresses(name)
			if err != nil {
				glog.Warningf("Unable to obtain external host address from cloud provider: %v", err)
			} else {
				for _, addr := range addrs {
					if addr.Type == api.NodeExternalIP {
						s.ExternalHost = addr.Address
					}
				}
			}
		}
	}

	config := &master.Config{
		Config: &genericapiserver.Config{
			StorageDestinations:       storageDestinations,
			StorageVersions:           storageVersions,
			ServiceClusterIPRange:     &n,
			EnableLogsSupport:         s.EnableLogsSupport,
			EnableUISupport:           true,
			EnableSwaggerSupport:      true,
			EnableProfiling:           s.EnableProfiling,
			EnableWatchCache:          s.EnableWatchCache,
			EnableIndex:               true,
			APIPrefix:                 s.APIPrefix,
			APIGroupPrefix:            s.APIGroupPrefix,
			CorsAllowedOriginList:     s.CorsAllowedOriginList,
			ReadWritePort:             s.SecurePort,
			PublicAddress:             s.AdvertiseAddress,
			Authenticator:             authenticator,
			SupportsBasicAuth:         len(s.BasicAuthFile) > 0,
			Authorizer:                authorizer,
			AdmissionControl:          admissionController,
			APIGroupVersionOverrides:  apiGroupVersionOverrides,
			MasterServiceNamespace:    s.MasterServiceNamespace,
			MasterCount:               s.MasterCount,
			ExternalHost:              s.ExternalHost,
			MinRequestTimeout:         s.MinRequestTimeout,
			ProxyDialer:               proxyDialerFn,
			ProxyTLSClientConfig:      proxyTLSClientConfig,
			ServiceNodePortRange:      s.ServiceNodePortRange,
			KubernetesServiceNodePort: s.KubernetesServiceNodePort,
			Serializer:                api.Codecs,
		},
		EnableCoreControllers:   true,
		DeleteCollectionWorkers: s.DeleteCollectionWorkers,
		EventTTL:                s.EventTTL,
		KubeletClient:           kubeletClient,

		Tunneler: tunneler,
	}

	if s.EnableWatchCache {
		cachesize.SetWatchCacheSizes(s.WatchCacheSizes)
	}

	m, err := master.New(config)
	if err != nil {
		return err
	}

	m.Run(s.ServerRunOptions)
	return nil
}
