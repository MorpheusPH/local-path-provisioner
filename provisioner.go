package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v8/controller"
)

type ActionType string

const (
	ActionTypeCreate = "create"
	ActionTypeDelete = "delete"
)

const (
	KeyNode = "kubernetes.io/hostname"

	NodeDefaultNonListedNodes = "DEFAULT_PATH_FOR_NON_LISTED_NODES"

	helperScriptDir     = "/script"
	helperDataVolName   = "data"
	helperScriptVolName = "script"

	envVolDir    = "VOL_DIR"
	envVolMode   = "VOL_MODE"
	envVolSize   = "VOL_SIZE_BYTES"
	envRegistry  = "REGISTRY"
	envStoreType = "STORAGE_TYPE"
	envREPOTAG   = "REPO_TAG"
)

const (
	defaultCmdTimeoutSeconds = 120
	defaultVolumeType        = "hostPath"
)

var (
	ConfigFileCheckInterval = 30 * time.Second

	HelperPodNameMaxLength = 128
)

type LocalPathProvisioner struct {
	ctx                context.Context
	kubeClient         *clientset.Clientset
	namespace          string
	helperImage        string
	serviceAccountName string

	config        *Config
	configData    *ConfigData
	configFile    string
	configMapName string
	configMutex   *sync.RWMutex
	helperPod     *v1.Pod
	modelCache    bool
	modelPath     string
	registry      string
	storeType     string
	defaultMount  string
	owner         string
}

type NodePathMapData struct {
	Node  string   `json:"node,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

type ConfigData struct {
	NodePathMap          []*NodePathMapData `json:"nodePathMap,omitempty"`
	CmdTimeoutSeconds    int                `json:"cmdTimeoutSeconds,omitempty"`
	SharedFileSystemPath string             `json:"sharedFileSystemPath,omitempty"`
}

type NodePathMap struct {
	Paths map[string]struct{}
}

type Config struct {
	NodePathMap          map[string]*NodePathMap
	CmdTimeoutSeconds    int
	SharedFileSystemPath string
}

type pvcMetadata struct {
	data        map[string]string
	labels      map[string]string
	annotations map[string]string
	emptyPath   bool
}

var pattern = regexp.MustCompile(`\${\.PVC\.((labels|annotations)\.(.*?)|.*?)}`)

func (meta *pvcMetadata) stringParser(str string) string {
	add_pvc_name := false
	result := pattern.FindAllStringSubmatch(str, -1)
	for _, r := range result {
		switch r[2] {
		case "labels":
			label, ok := meta.labels[r[3]]
			if !ok {
				add_pvc_name = true
			}
			str = strings.ReplaceAll(str, r[0], label)
			meta.emptyPath = false
		case "annotations":
			annotation, ok := meta.annotations[r[3]]
			if !ok {
				add_pvc_name = true
			}
			str = strings.ReplaceAll(str, r[0], annotation)
			meta.emptyPath = false
		default:
			str = strings.ReplaceAll(str, r[0], meta.data[r[1]])
		}
	}
	if add_pvc_name {
		str = filepath.Join(str, meta.data["name"])
	}
	logrus.Infof("path %s", str)
	return str
}

func NewProvisioner(ctx context.Context, kubeClient *clientset.Clientset,
	configFile, namespace, helperImage, configMapName, serviceAccountName, helperPodYaml string) (*LocalPathProvisioner, error) {
	p := &LocalPathProvisioner{
		ctx: ctx,

		kubeClient:         kubeClient,
		namespace:          namespace,
		helperImage:        helperImage,
		serviceAccountName: serviceAccountName,

		// config will be updated shortly by p.refreshConfig()
		config:        nil,
		configFile:    configFile,
		configData:    nil,
		configMapName: configMapName,
		configMutex:   &sync.RWMutex{},
		modelPath:     "",
		registry:      "",
		storeType:     "",
		defaultMount:  "/model",
		owner:         "public",
	}
	var err error
	p.helperPod, err = loadHelperPodFile(helperPodYaml)
	if err != nil {
		return nil, err
	}
	if err := p.refreshConfig(); err != nil {
		return nil, err
	}
	p.watchAndRefreshConfig()
	return p, nil
}

func (p *LocalPathProvisioner) refreshConfig() error {
	p.configMutex.Lock()
	defer p.configMutex.Unlock()

	configData, err := loadConfigFile(p.configFile)
	if err != nil {
		return err
	}
	// no need to update
	if reflect.DeepEqual(configData, p.configData) {
		return nil
	}
	config, err := canonicalizeConfig(configData)
	if err != nil {
		return err
	}
	// only update the config if the new config file is valid
	p.configData = configData
	p.config = config

	output, err := json.Marshal(p.configData)
	if err != nil {
		return err
	}
	logrus.Debugf("Applied config: %v", string(output))

	return err
}

func (p *LocalPathProvisioner) watchAndRefreshConfig() {
	go func() {
		ticker := time.NewTicker(ConfigFileCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := p.refreshConfig(); err != nil {
					logrus.Errorf("failed to load the new config file: %v", err)
				}
			case <-p.ctx.Done():
				logrus.Infof("stop watching config file")
				return
			}
		}
	}()
}

func (p *LocalPathProvisioner) getPathOnNode(node string) (string, error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()

	if p.config == nil {
		return "", fmt.Errorf("no valid config available")
	}

	c := p.config
	sharedFS, err := p.isSharedFilesystem()
	if err != nil {
		return "", err
	}
	if sharedFS {
		// we are ignoring 'node' and returning shared FS path
		return c.SharedFileSystemPath, nil
	}
	// we are working with local FS
	npMap := c.NodePathMap[node]
	if npMap == nil {
		npMap = c.NodePathMap[NodeDefaultNonListedNodes]
		if npMap == nil {
			return "", fmt.Errorf("config doesn't contain node %v, and no %v available", node, NodeDefaultNonListedNodes)
		}
		logrus.Debugf("config doesn't contain node %v, use %v instead", node, NodeDefaultNonListedNodes)
	}
	paths := npMap.Paths
	if len(paths) == 0 {
		return "", fmt.Errorf("no local path available on node %v", node)
	}
	// if a particular path was requested by storage class
	// if requestedPath != "" {
	// 	if _, ok := paths[requestedPath]; !ok {
	// 		return "", fmt.Errorf("config doesn't contain path %v on node %v", requestedPath, node)
	// 	}
	// 	return requestedPath, nil
	// }
	// if no particular path was requested, choose a random one
	path := ""
	for path = range paths {
		break
	}
	return path, nil
}

func (p *LocalPathProvisioner) isSharedFilesystem() (bool, error) {
	p.configMutex.RLock()
	defer p.configMutex.RUnlock()

	if p.config == nil {
		return false, fmt.Errorf("no valid config available")
	}

	c := p.config
	if (c.SharedFileSystemPath != "") && (len(c.NodePathMap) != 0) {
		return false, fmt.Errorf("both nodePathMap and sharedFileSystemPath are defined. Please make sure only one is in use")
	}

	if len(c.NodePathMap) != 0 {
		return false, nil
	}

	if c.SharedFileSystemPath != "" {
		return true, nil
	}

	return false, fmt.Errorf("both nodePathMap and sharedFileSystemPath are unconfigured")
}

func (p *LocalPathProvisioner) Provision(ctx context.Context, opts pvController.ProvisionOptions) (*v1.PersistentVolume, pvController.ProvisioningState, error) {
	pvc := opts.PVC
	node := opts.SelectedNode
	storageClass := opts.StorageClass
	sharedFS, err := p.isSharedFilesystem()
	if err != nil {
		return nil, pvController.ProvisioningFinished, err
	}
	if !sharedFS {
		if pvc.Spec.Selector != nil {
			return nil, pvController.ProvisioningFinished, fmt.Errorf("claim.Spec.Selector is not supported")
		}
		for _, accessMode := range pvc.Spec.AccessModes {
			if accessMode != v1.ReadWriteOnce {
				return nil, pvController.ProvisioningFinished, fmt.Errorf("Only support ReadWriteOnce access mode")
			}
		}
		if node == nil {
			return nil, pvController.ProvisioningFinished, fmt.Errorf("configuration error, no node was specified")
		}
	}

	nodeName := ""
	if node != nil {
		// This clause works only with sharedFS
		nodeName = node.Name
	}
	// var requestedPath string
	// if storageClass.Parameters != nil {
	// if _, ok := storageClass.Parameters["nodePath"]; ok {
	// 	requestedPath = storageClass.Parameters["nodePath"]
	// }
	// }
	basePath, err := p.getPathOnNode(nodeName)
	if err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	name := opts.PVName
	folderName := strings.Join([]string{name, opts.PVC.Namespace, opts.PVC.Name}, "_")
	path := filepath.Join(basePath, folderName)

	metadata := &pvcMetadata{
		data: map[string]string{
			"name":      pvc.Name,
			"namespace": pvc.Namespace,
		},
		labels:      pvc.Labels,
		annotations: pvc.Annotations,
		emptyPath:   true,
	}

	owner, exists := pvc.Labels["owner"]
	if exists {
		p.owner = owner
	}

	modelCache := false
	if storageClass.Parameters != nil {
		isModelCache, exists := storageClass.Parameters["modelCache"]
		if exists {
			modelCache, err = strconv.ParseBool(isModelCache)
			if err != nil {
				return nil, pvController.ProvisioningFinished, err
			}
			registry, exists := storageClass.Parameters["registry"]
			if !exists {
				return nil, pvController.ProvisioningFinished, fmt.Errorf("The registry parameter must be set")
			}
			p.registry = registry

			storeType, exists := storageClass.Parameters["storeType"]
			if !exists {
				return nil, pvController.ProvisioningFinished, fmt.Errorf("The storeType parameter must be set")
			}
			p.storeType = storeType
		}
		pathPattern, exists := opts.StorageClass.Parameters["pathPattern"]
		if exists {
			customPath := metadata.stringParser(pathPattern)
			p.modelPath = customPath
			if !metadata.emptyPath && customPath != "" {
				path = filepath.Join(basePath, customPath)
			}
		}
	}
	if nodeName == "" {
		logrus.Infof("Creating volume %v at %v", name, path)
	} else {
		logrus.Infof("Creating volume %v at %v:%v", name, nodeName, path)
	}

	storage := pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	provisionCmd := []string{"/bin/sh", "/script/setup"}
	if err := p.createHelperPod(ActionTypeCreate, provisionCmd, volumeOptions{
		Name:        name,
		Path:        path,
		Mode:        *pvc.Spec.VolumeMode,
		SizeInBytes: storage.Value(),
		Node:        nodeName,
		ModelCache:  modelCache,
	}, pvc.Annotations); err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	fs := v1.PersistentVolumeFilesystem

	var pvs v1.PersistentVolumeSource
	var volumeType string
	if dVal, ok := opts.StorageClass.GetAnnotations()["defaultVolumeType"]; ok {
		volumeType = dVal
	} else {
		volumeType = defaultVolumeType
	}
	if val, ok := opts.PVC.GetAnnotations()["volumeType"]; ok {
		volumeType = val
	}
	pvs, err = createPersistentVolumeSource(volumeType, path)
	if err != nil {
		return nil, pvController.ProvisioningFinished, err
	}

	var nodeAffinity *v1.VolumeNodeAffinity
	if sharedFS {
		// If the same filesystem is mounted across all nodes, we don't need
		// affinity, as path is accessible from any node
		// nodeAffinity = nil
		nodeAffinity = &v1.VolumeNodeAffinity{
			Required: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      KeyNode,
								Operator: v1.NodeSelectorOpExists,
							},
						},
					},
				},
			},
		}
	} else {
		valueNode, ok := node.GetLabels()[KeyNode]
		if !ok {
			valueNode = nodeName
		}
		nodeAffinity = &v1.VolumeNodeAffinity{
			Required: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      KeyNode,
								Operator: v1.NodeSelectorOpIn,
								Values: []string{
									valueNode,
								},
							},
						},
					},
				},
			},
		}
	}
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *opts.StorageClass.ReclaimPolicy,
			AccessModes:                   pvc.Spec.AccessModes,
			VolumeMode:                    &fs,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: pvs,
			NodeAffinity:           nodeAffinity,
		},
	}, pvController.ProvisioningFinished, nil
}

func (p *LocalPathProvisioner) Delete(ctx context.Context, pv *v1.PersistentVolume) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()
	path, node, err := p.getPathAndNodeForPV(pv)
	if err != nil {
		return err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		if node == "" {
			logrus.Infof("Deleting volume %v at %v", pv.Name, path)
		} else {
			logrus.Infof("Deleting volume %v at %v:%v", pv.Name, node, path)
		}
		storage := pv.Spec.Capacity[v1.ResourceName(v1.ResourceStorage)]
		cleanupCmd := []string{"/bin/sh", "/script/teardown"}
		if err := p.createHelperPod(ActionTypeDelete, cleanupCmd, volumeOptions{
			Name:        pv.Name,
			Path:        path,
			Mode:        *pv.Spec.VolumeMode,
			SizeInBytes: storage.Value(),
			Node:        node,
		}, nil); err != nil {
			logrus.Infof("clean up volume %v failed: %v", pv.Name, err)
			return err
		}
		return nil
	}
	logrus.Infof("Retained volume %v", pv.Name)
	return nil
}

func (p *LocalPathProvisioner) getPathAndNodeForPV(pv *v1.PersistentVolume) (path, node string, err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()

	volumeSource := pv.Spec.PersistentVolumeSource
	if volumeSource.HostPath != nil && volumeSource.Local == nil {
		path = volumeSource.HostPath.Path
	} else if volumeSource.Local != nil && volumeSource.HostPath == nil {
		path = volumeSource.Local.Path
	} else {
		return "", "", fmt.Errorf("no path set")
	}

	sharedFS, err := p.isSharedFilesystem()
	if err != nil {
		return "", "", err
	}

	if sharedFS {
		// We don't have affinity and can use any node
		return path, "", nil
	}

	// Dealing with local filesystem

	nodeAffinity := pv.Spec.NodeAffinity
	if nodeAffinity == nil {
		return "", "", fmt.Errorf("no NodeAffinity set")
	}
	required := nodeAffinity.Required
	if required == nil {
		return "", "", fmt.Errorf("no NodeAffinity.Required set")
	}

	node = ""
	for _, selectorTerm := range required.NodeSelectorTerms {
		for _, expression := range selectorTerm.MatchExpressions {
			if expression.Key == KeyNode && expression.Operator == v1.NodeSelectorOpIn {
				if len(expression.Values) != 1 {
					return "", "", fmt.Errorf("multiple values for the node affinity")
				}
				node = expression.Values[0]
				break
			}
		}
		if node != "" {
			break
		}
	}
	if node == "" {
		return "", "", fmt.Errorf("cannot find affinited node")
	}
	return path, node, nil
}

type volumeOptions struct {
	Name        string
	Path        string
	Mode        v1.PersistentVolumeMode
	SizeInBytes int64
	Node        string
	ModelCache  bool
}

func (p *LocalPathProvisioner) createHelperPod(action ActionType, cmd []string, o volumeOptions, annotation map[string]string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to %v volume %v", action, o.Name)
	}()
	sharedFS, err := p.isSharedFilesystem()
	if err != nil {
		return err
	}
	if o.Name == "" || o.Path == "" || (!sharedFS && o.Node == "") {
		return fmt.Errorf("invalid empty name or path or node")
	}
	if !filepath.IsAbs(o.Path) {
		return fmt.Errorf("volume path %s is not absolute", o.Path)
	}
	o.Path = filepath.Clean(o.Path)
	parentDir, volumeDir := filepath.Split(o.Path)
	hostPathType := v1.HostPathDirectoryOrCreate
	var setup v1.KeyToPath
	var cmdVolume v1.Volume
	if action == ActionTypeCreate {
		if o.ModelCache {
			setup = v1.KeyToPath{
				Key:  "setupcache",
				Path: "setup",
			}
		} else {
			setup = v1.KeyToPath{
				Key:  "setup",
				Path: "setup",
			}
		}
		cmdVolume = v1.Volume{
			Name: helperScriptVolName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: p.configMapName,
					},
					Items: []v1.KeyToPath{
						setup,
					},
				},
			},
		}
	} else {
		cmdVolume = v1.Volume{
			Name: helperScriptVolName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: p.configMapName,
					},
					Items: []v1.KeyToPath{
						{
							Key:  "teardown",
							Path: "teardown",
						},
					},
				},
			},
		}
	}
	lpvVolumes := []v1.Volume{
		{
			Name: helperDataVolName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: parentDir,
					Type: &hostPathType,
				},
			},
		},
	}
	lpvVolumes = append(lpvVolumes, cmdVolume)
	lpvTolerations := []v1.Toleration{
		{
			Operator: v1.TolerationOpExists,
		},
	}
	helperPod := p.helperPod.DeepCopy()

	scriptMount := addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperScriptVolName, helperScriptDir)
	scriptMount.MountPath = helperScriptDir
	var dataMount *v1.VolumeMount
	var vol_dir string
	hash := calculatorSha256(o.Path)
	if o.ModelCache {
		helperPod.Name = ("cache-" + string(action) + "-" + o.Node + "-" + hash)
		basePath, _ := p.getPathOnNode(o.Node)
		modelPath := strings.TrimPrefix(parentDir, basePath)
		dataMount = addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperDataVolName, filepath.Join(p.defaultMount, modelPath))
		vol_dir = filepath.Join(p.defaultMount, modelPath, volumeDir)
	} else {
		helperPod.Name = (helperPod.Name + "-" + string(action) + "-" + o.Name)
		dataMount = addVolumeMount(&helperPod.Spec.Containers[0].VolumeMounts, helperDataVolName, parentDir)
		vol_dir = filepath.Join(parentDir, volumeDir)
	}
	parentDir = dataMount.MountPath
	parentDir = strings.TrimSuffix(parentDir, string(filepath.Separator))
	volumeDir = strings.TrimSuffix(volumeDir, string(filepath.Separator))
	if parentDir == "" || volumeDir == "" || !filepath.IsAbs(parentDir) {
		// it covers the `/` case
		return fmt.Errorf("invalid path %v for %v: cannot find parent dir or volume dir or parent dir is relative", action, o.Path)
	}

	env := []v1.EnvVar{
		{Name: envVolDir, Value: vol_dir},
		{Name: envVolMode, Value: string(o.Mode)},
		{Name: envVolSize, Value: strconv.FormatInt(o.SizeInBytes, 10)},
	}
	if o.ModelCache {
		cacheEnv := []v1.EnvVar{
			{Name: envRegistry, Value: p.registry},
			{Name: envStoreType, Value: p.storeType},
			// {Name: envREPOTAG, Value: fmt.Sprintf("%s:%s", p.modelPath, "latest")},
		}
		if annotation != nil {
			registry, ok := annotation["model/registry"]
			if ok {
				registryEnv := v1.EnvVar{
					Name:  envREPOTAG,
					Value: registry,
				}
				cacheEnv = append(cacheEnv, registryEnv)
			}
		}
		env = append(env, cacheEnv...)
	}

	// use different name for helper pods
	// https://github.com/rancher/local-path-provisioner/issues/154
	// helperPod.Name = (helperPod.Name + "-" + string(action) + "-" + o.Name)
	if len(helperPod.Name) > HelperPodNameMaxLength {
		helperPod.Name = helperPod.Name[:HelperPodNameMaxLength]
	}
	helperPod.Namespace = p.namespace
	if o.Node != "" {
		helperPod.Spec.NodeName = o.Node
	}
	privileged := true
	helperPod.Spec.ServiceAccountName = p.serviceAccountName
	helperPod.Spec.RestartPolicy = v1.RestartPolicyNever
	helperPod.Spec.Tolerations = append(helperPod.Spec.Tolerations, lpvTolerations...)
	helperPod.Spec.Volumes = append(helperPod.Spec.Volumes, lpvVolumes...)
	helperPod.Spec.Containers[0].Command = cmd
	helperPod.Spec.Containers[0].Env = append(helperPod.Spec.Containers[0].Env, env...)
	helperPod.Spec.Containers[0].Args = []string{"-p", vol_dir,
		"-s", strconv.FormatInt(o.SizeInBytes, 10),
		"-m", string(o.Mode)}
	helperPod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
		Privileged: &privileged,
	}
	if o.ModelCache {
		helperPod.Spec.Containers[0].Image = p.helperImage
	}

	// If it already exists due to some previous errors, the pod will be cleaned up later automatically
	// https://github.com/rancher/local-path-provisioner/issues/27
	helperPodExists := false
	_, err = p.kubeClient.CoreV1().Pods(p.namespace).Get(context.TODO(), helperPod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		helperPodExists = false
	} else if err != nil {
		log.Fatalf("Error getting helperPod: %v", err)
	} else {
		helperPodExists = true
		fmt.Printf("helperPod %s exists in namespace %s\n, skip create  helpPod", helperPod.Name, p.namespace)
	}

	if !helperPodExists {
		logrus.Infof("create the helper pod %s into %s", helperPod.Name, p.namespace)
		_, err = p.kubeClient.CoreV1().Pods(p.namespace).Create(context.TODO(), helperPod, metav1.CreateOptions{})
		if err != nil && !k8serror.IsAlreadyExists(err) {
			return err
		}

		defer func() {
			e := p.kubeClient.CoreV1().Pods(p.namespace).Delete(context.TODO(), helperPod.Name, metav1.DeleteOptions{})
			if e != nil {
				logrus.Errorf("unable to delete the helper pod: %v", e)
			}
		}()
	}

	completed := false
	for i := 0; i < p.config.CmdTimeoutSeconds; i++ {
		if pod, err := p.kubeClient.CoreV1().Pods(p.namespace).Get(context.TODO(), helperPod.Name, metav1.GetOptions{}); err != nil {
			return err
		} else if pod.Status.Phase == v1.PodSucceeded {
			completed = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !completed {
		return fmt.Errorf("create process timeout after %v seconds", p.config.CmdTimeoutSeconds)
	}

	if o.Node == "" {
		logrus.Infof("Volume %v has been %vd on %v", o.Name, action, o.Path)
	} else {
		logrus.Infof("Volume %v has been %vd on %v:%v", o.Name, action, o.Node, o.Path)
	}
	return nil
}

func addVolumeMount(mounts *[]v1.VolumeMount, name, mountPath string) *v1.VolumeMount {
	for i, m := range *mounts {
		if m.Name == name {
			if m.MountPath == "" {
				(*mounts)[i].MountPath = mountPath
			}
			return &(*mounts)[i]
		}
	}
	*mounts = append(*mounts, v1.VolumeMount{Name: name, MountPath: mountPath})
	return &(*mounts)[len(*mounts)-1]
}

func isJSONFile(configFile string) bool {
	return strings.HasSuffix(configFile, ".json")
}

func unmarshalFromString(configFile string) (*ConfigData, error) {
	var data ConfigData
	if err := json.Unmarshal([]byte(configFile), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func loadConfigFile(configFile string) (cfgData *ConfigData, err error) {
	defer func() {
		err = errors.Wrapf(err, "fail to load config file %v", configFile)
	}()

	if !isJSONFile(configFile) {
		return unmarshalFromString(configFile)
	}

	f, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var data ConfigData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}
	return &data, nil
}

func canonicalizeConfig(data *ConfigData) (cfg *Config, err error) {
	defer func() {
		err = errors.Wrapf(err, "config canonicalization failed")
	}()
	cfg = &Config{}
	cfg.SharedFileSystemPath = data.SharedFileSystemPath
	cfg.NodePathMap = map[string]*NodePathMap{}
	for _, n := range data.NodePathMap {
		if cfg.NodePathMap[n.Node] != nil {
			return nil, fmt.Errorf("duplicate node %v", n.Node)
		}
		npMap := &NodePathMap{Paths: map[string]struct{}{}}
		cfg.NodePathMap[n.Node] = npMap
		for _, p := range n.Paths {
			if p[0] != '/' {
				return nil, fmt.Errorf("path must start with / for path %v on node %v", p, n.Node)
			}
			path, err := filepath.Abs(p)
			if err != nil {
				return nil, err
			}
			if path == "/" {
				return nil, fmt.Errorf("cannot use root ('/') as path on node %v", n.Node)
			}
			if _, ok := npMap.Paths[path]; ok {
				return nil, fmt.Errorf("duplicate path %v on node %v", p, n.Node)
			}
			npMap.Paths[path] = struct{}{}
		}
	}
	if data.CmdTimeoutSeconds > 0 {
		cfg.CmdTimeoutSeconds = data.CmdTimeoutSeconds
	} else {
		cfg.CmdTimeoutSeconds = defaultCmdTimeoutSeconds
	}
	return cfg, nil
}

func createPersistentVolumeSource(volumeType string, path string) (pvs v1.PersistentVolumeSource, err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to create persistent volume source")
	}()

	switch strings.ToLower(volumeType) {
	case "local":
		pvs = v1.PersistentVolumeSource{
			Local: &v1.LocalVolumeSource{
				Path: path,
			},
		}
	case "hostpath":
		hostPathType := v1.HostPathDirectoryOrCreate
		pvs = v1.PersistentVolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: path,
				Type: &hostPathType,
			},
		}
	default:
		return pvs, fmt.Errorf("\"%s\" is not a recognised volume type", volumeType)
	}

	return pvs, nil
}

func calculatorSha256(path string) string {
	h := sha256.New()
	h.Write([]byte(path))
	bs := h.Sum(nil)
	hash := fmt.Sprintf("%x", bs)[0:8]
	return hash
}
