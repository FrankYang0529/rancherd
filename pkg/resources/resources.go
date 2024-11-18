package resources

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	v1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/system-agent/pkg/applyinator"
	"github.com/rancher/wrangler/pkg/randomtoken"
	"github.com/rancher/wrangler/pkg/yaml"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rancher/rancherd/pkg/config"
	"github.com/rancher/rancherd/pkg/images"
	"github.com/rancher/rancherd/pkg/kubectl"
	"github.com/rancher/rancherd/pkg/self"
	"github.com/rancher/rancherd/pkg/versions"
)

const (
	localRKEStateSecretType = "rke.cattle.io/cluster-state"
)

func writeCattleID(id string) error {
	if err := os.MkdirAll("/etc/rancher", 0755); err != nil {
		return fmt.Errorf("mkdir /etc/rancher: %w", err)
	}
	if err := os.MkdirAll("/etc/rancher/agent", 0700); err != nil {
		return fmt.Errorf("mkdir /etc/rancher/agent: %w", err)
	}
	return ioutil.WriteFile("/etc/rancher/agent/cattle-id", []byte(id), 0400)
}

func getCattleID() (string, error) {
	data, err := ioutil.ReadFile("/etc/rancher/agent/cattle-id")
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		id, err = randomtoken.Generate()
		if err != nil {
			return "", err
		}
		return id, writeCattleID(id)
	}
	return id, nil
}

func ToBootstrapFile(config *config.Config, path string) (*applyinator.File, error) {
	nodeName := config.NodeName
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("looking up hostname: %w", err)
		}
		nodeName = strings.Split(hostname, ".")[0]
	}

	k8sVersion, err := versions.K8sVersion(config.KubernetesVersion)
	if err != nil {
		return nil, err
	}

	token := config.Token
	if token == "" {
		token, err = randomtoken.Generate()
		if err != nil {
			return nil, err
		}
	}

	resources := config.Resources
	return ToFile(append(resources, v1.GenericMap{
		Data: map[string]interface{}{
			"kind":       "Node",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name": nodeName,
				"labels": map[string]interface{}{
					"node-role.kubernetes.io/etcd": "true",
				},
			},
		},
	}, v1.GenericMap{
		Data: map[string]interface{}{
			"kind":       "Namespace",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name": "fleet-local",
			},
		},
	}, v1.GenericMap{
		Data: map[string]interface{}{
			"kind":       "Cluster",
			"apiVersion": "provisioning.cattle.io/v1",
			"metadata": map[string]interface{}{
				"name":      "local",
				"namespace": "fleet-local",
				"labels": map[string]interface{}{
					"provisioning.cattle.io/management-cluster-name": "local",
				},
			},
			"spec": map[string]interface{}{
				"kubernetesVersion": k8sVersion,
				// Rancher needs a non-null rkeConfig to apply system-upgrade-controller managed chart.
				"rkeConfig": map[string]interface{}{},
			},
		},
	}, v1.GenericMap{
		Data: map[string]interface{}{
			"kind":       "Secret",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "local-rke-state",
				"namespace": "fleet-local",
			},
			"type": localRKEStateSecretType,
			"data": map[string]interface{}{
				"serverToken": []byte(token),
				"agentToken":  []byte(token),
			},
		},
	}), path)
}
func ToFile(resources []v1.GenericMap, path string) (*applyinator.File, error) {
	if len(resources) == 0 {
		return nil, nil
	}

	var objs []runtime.Object
	for _, resource := range resources {
		objs = append(objs, &unstructured.Unstructured{
			Object: resource.Data,
		})
	}

	data, err := yaml.ToBytes(objs)
	if err != nil {
		return nil, err
	}

	return &applyinator.File{
		Content: base64.StdEncoding.EncodeToString(data),
		Path:    path,
	}, nil
}

func GetBootstrapManifests(dataDir string) string {
	return fmt.Sprintf("%s/bootstrapmanifests/rancherd.yaml", dataDir)
}

func ToInstruction(imageOverride, systemDefaultRegistry, k8sVersion, dataDir string) (*applyinator.Instruction, error) {
	bootstrap := GetBootstrapManifests(dataDir)
	cmd, err := self.Self()
	if err != nil {
		return nil, fmt.Errorf("resolving location of %s: %w", os.Args[0], err)
	}
	return &applyinator.Instruction{
		Name:       "bootstrap",
		SaveOutput: true,
		Image:      images.GetInstallerImage(imageOverride, systemDefaultRegistry, k8sVersion),
		Args:       []string{"retry", kubectl.Command(k8sVersion), "apply", "--validate=false", "-f", bootstrap},
		Command:    cmd,
		Env:        kubectl.Env(k8sVersion),
	}, nil
}

func ToSystemAgentUpgraderFile(path string) (*applyinator.File, error) {
	return ToFile([]v1.GenericMap{
		{
			Data: map[string]interface{}{
				"apiVersion": "upgrade.cattle.io/v1",
				"kind":       "Plan",
				"metadata": map[string]interface{}{
					"name":      "system-agent-upgrader",
					"namespace": "cattle-system",
				},
				"spec": map[string]interface{}{
					"concurrency": 10,
					"nodeSelector": map[string]interface{}{
						"matchExpressions": []map[string]interface{}{
							{
								"key":      "kubernetes.io/os",
								"operator": "In",
								"values": []string{
									"linux",
								},
							},
						},
					},
					"serviceAccountName": "system-upgrade-controller",
					"tolerations": []map[string]interface{}{
						{
							"operator": "Exists",
						},
					},
					"upgrade": map[string]interface{}{
						"envs": []map[string]interface{}{
							{
								"name":  "CATTLE_AGENT_LOGLEVEL",
								"value": "debug",
							},
							{
								"name":  "CATTLE_REMOTE_ENABLED",
								"value": "false",
							},
							{
								"name":  "CATTLE_LOCAL_ENABLED",
								"value": "true",
							},
						},
						"image": "rancher/system-agent",
					},
					"version": "v0.3.9-suc",
				},
			},
		},
	}, path)
}

func GetSystemAgentUpgraderManifests(dataDir string) string {
	return fmt.Sprintf("%s/bootstrapmanifests/system-agent-upgrader.yaml", dataDir)
}

func ToSystemAgentUpgraderInstruction(imageOverride, systemDefaultRegistry, k8sVersion, dataDir string) (*applyinator.Instruction, error) {
	systemAgentUpgrader := GetSystemAgentUpgraderManifests(dataDir)
	cmd, err := self.Self()
	if err != nil {
		return nil, fmt.Errorf("resolving location of %s: %w", os.Args[0], err)
	}
	return &applyinator.Instruction{
		Name:       "system-agent-upgrader",
		SaveOutput: true,
		Image:      images.GetInstallerImage(imageOverride, systemDefaultRegistry, k8sVersion),
		Args:       []string{"retry", kubectl.Command(k8sVersion), "apply", "--validate=false", "-f", systemAgentUpgrader},
		Command:    cmd,
		Env:        kubectl.Env(k8sVersion),
	}, nil
}
