// Copyright 2019 Layer5.io
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package linkerd

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	"github.com/alecthomas/template"
	"github.com/layer5io/meshery-linkerd/meshes"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	kubeerror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/restmapper"
)

// CreateMeshInstance - creates a mesh adapter instance
func (iClient *Client) CreateMeshInstance(_ context.Context, k8sReq *meshes.CreateMeshInstanceRequest) (*meshes.CreateMeshInstanceResponse, error) {
	var k8sConfig []byte
	contextName := ""
	if k8sReq != nil {
		k8sConfig = k8sReq.K8SConfig
		contextName = k8sReq.ContextName
	}
	// logrus.Debugf("received k8sConfig: %s", k8sConfig)
	logrus.Debugf("received contextName: %s", contextName)

	ic, err := newClient(k8sConfig, contextName)
	if err != nil {
		err = errors.Wrapf(err, "unable to create a new linkerd client")
		logrus.Error(err)
		return nil, err
	}
	iClient.k8sClientset = ic.k8sClientset
	iClient.k8sDynamicClient = ic.k8sDynamicClient
	iClient.eventChan = make(chan *meshes.EventsResponse, 100)
	iClient.config = ic.config
	iClient.contextName = ic.contextName
	iClient.kubeconfig = ic.kubeconfig
	return &meshes.CreateMeshInstanceResponse{}, nil
}

func (iClient *Client) getResource(ctx context.Context, res schema.GroupVersionResource, data *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var data1 *unstructured.Unstructured
	var err error
	logrus.Debugf("getResource data: %+#v", data)
	logrus.Debugf("getResource res: %+#v", res)
	data1, err = iClient.k8sDynamicClient.Resource(res).Namespace(data.GetNamespace()).Get(data.GetName(), metav1.GetOptions{})
	if err != nil {
		err = errors.Wrap(err, "unable to retrieve the resource with a matching name, attempting operation without namespace")
		logrus.Warn(err)

		data1, err = iClient.k8sDynamicClient.Resource(res).Get(data.GetName(), metav1.GetOptions{})
		if err != nil {
			err = errors.Wrap(err, "unable to retrieve the resource with a matching name, while attempting to apply the config")
			logrus.Error(err)
			return nil, err
		}
	}
	logrus.Infof("Retrieved Resource of type: %s and name: %s", data.GetKind(), data.GetName())
	return data1, nil
}

// updateResource - updates a Kubernetes resource
func (iClient *Client) updateResource(ctx context.Context, res schema.GroupVersionResource, data *unstructured.Unstructured) error {
	if _, err := iClient.k8sDynamicClient.Resource(res).Namespace(data.GetNamespace()).Update(data, metav1.UpdateOptions{}); err != nil {
		err = errors.Wrap(err, "unable to update resource with the given name, attempting operation without namespace")
		logrus.Warn(err)

		if _, err = iClient.k8sDynamicClient.Resource(res).Update(data, metav1.UpdateOptions{}); err != nil {
			err = errors.Wrap(err, "unable to update resource with the given name, while attempting to apply the config")
			logrus.Error(err)
			return err
		}
	}
	logrus.Infof("Updated Resource of type: %s and name: %s", data.GetKind(), data.GetName())
	return nil
}

// MeshName just returns the name of the mesh the client is representing
func (iClient *Client) MeshName(context.Context, *meshes.MeshNameRequest) (*meshes.MeshNameResponse, error) {
	return &meshes.MeshNameResponse{Name: "Linkerd"}, nil
}

func (iClient *Client) labelNamespaceForAutoInjection(ctx context.Context, namespace string) error {
	ns := &unstructured.Unstructured{}
	res := schema.GroupVersionResource{
		Version:  "v1",
		Resource: "namespaces",
	}
	ns.SetName(namespace)
	ns, err := iClient.getResource(ctx, res, ns)
	if err != nil {
		if strings.HasSuffix(err.Error(), "not found") {
			if err = iClient.createNamespace(ctx, namespace); err != nil {
				return err
			}

			ns := &unstructured.Unstructured{}
			ns.SetName(namespace)
			ns, err = iClient.getResource(ctx, res, ns)
			if err != nil {
				logrus.Debugf("Error getting namespace %s", ns.GetName())
				return err
			}
		} else {
			return err
		}
	}
	logrus.Debugf("retrieved namespace: %+#v", ns)
	if ns == nil {
		ns = &unstructured.Unstructured{}
		ns.SetName(namespace)
	}
	ns.SetAnnotations(map[string]string{
		"linkerd.io/inject": "enabled",
	})
	err = iClient.updateResource(ctx, res, ns)
	if err != nil {
		return err
	}
	return nil
}

// executeInstall - initiates provisioning of an instance of Linkerd
func (iClient *Client) executeInstall(ctx context.Context, arReq *meshes.ApplyRuleRequest) error {
	var tmpKubeConfigFileLoc = path.Join(os.TempDir(), fmt.Sprintf("kubeconfig_%d", time.Now().UnixNano()))
	err := os.Setenv("KUBECONFIG", tmpKubeConfigFileLoc)
	if err != nil {
		return err
	}

	// -L <namespace> --context <context name> --kubeconfig <file path>
	// logrus.Debugf("about to write kubeconfig to file: %s", iClient.kubeconfig)
	if err := ioutil.WriteFile(tmpKubeConfigFileLoc, iClient.kubeconfig, 0600); err != nil {
		return err
	}

	args1 := []string{"--linkerd-namespace", arReq.Namespace}
	if iClient.contextName != "" {
		args1 = append(args1, "--context", iClient.contextName)
	}
	args1 = append(args1, "--kubeconfig", tmpKubeConfigFileLoc)

	preCheck := append(args1, "check", "--pre")
	_, _, err = iClient.execute(preCheck...)
	if err != nil {
		return err
	}

	installArgs := append(args1, "install", "--ignore-cluster")
	yamlFileContents, er, err := iClient.execute(installArgs...)
	if err != nil {
		return err
	}
	if er != "" {
		err = fmt.Errorf("received error while attempting to prepare install yaml: %s", er)
		logrus.Error(err)
		return err
	}
	if err := iClient.applyConfigChange(ctx, yamlFileContents, arReq.Namespace, arReq.DeleteOp); err != nil {
		return err
	}

	err = os.Unsetenv("KUBECONFIG")
	if err != nil {
		return err
	}

	return nil
}

// executeTemplate - installs sample applications or other Kubernetes manifests
func (iClient *Client) executeTemplate(ctx context.Context, username, namespace, templateName string) (string, error) {
	tmpl, err := template.ParseFiles(path.Join("linkerd", "config_templates", templateName))
	if err != nil {
		err = errors.Wrapf(err, "unable to parse template")
		logrus.Error(err)
		return "", err
	}
	buf := bytes.NewBufferString("")
	err = tmpl.Execute(buf, map[string]string{
		"user_name": username,
		"namespace": namespace,
	})
	if err != nil {
		err = errors.Wrapf(err, "unable to execute template")
		logrus.Error(err)
		return "", err
	}
	return buf.String(), nil
}

// createNamespace - will create a new K8s namespace if one does not already exisst
func (iClient *Client) createNamespace(ctx context.Context, namespace string) error {
	logrus.Debugf("creating namespace: %s", namespace)
	yamlFileContents, err := iClient.executeTemplate(ctx, "", namespace, "namespace.yml")
	if err != nil {
		return err
	}
	if err := iClient.applyConfigChange(ctx, yamlFileContents, namespace, false); err != nil {
		return err
	}
	return nil
}

// ApplyOperation is a method invoked to apply a particular operation on the mesh in a namespace
func (iClient *Client) ApplyOperation(ctx context.Context, arReq *meshes.ApplyRuleRequest) (*meshes.ApplyRuleResponse, error) {
	if arReq == nil {
		return nil, errors.New("mesh client has not been created")
	}

	op, ok := supportedOps[arReq.OpName]
	if !ok {
		return nil, fmt.Errorf("operation id: %s, error: %s is not a valid operation name", arReq.OperationId, arReq.OpName)
	}

	if arReq.OpName == customOpCommand && arReq.CustomBody == "" {
		return nil, fmt.Errorf("operation id: %s, error: yaml body is empty for %s operation", arReq.OperationId, arReq.OpName)
	}

	var yamlFileContents string
	var appName, svcName string
	var err error

	switch arReq.OpName {
	case customOpCommand:
		yamlFileContents = arReq.CustomBody
	case installLinkerdCommand:
		go func() {
			opName1 := "deploying"
			if arReq.DeleteOp {
				opName1 = "removing"
			}
			if err := iClient.executeInstall(ctx, arReq); err != nil {
				iClient.eventChan <- &meshes.EventsResponse{
					OperationId: arReq.OperationId,
					EventType:   meshes.EventType_ERROR,
					Summary:     fmt.Sprintf("Error while %s Linkerd", opName1),
					Details:     err.Error(),
				}
				return
			}
			opName := "deployed"
			if arReq.DeleteOp {
				opName = "removed"
			}
			logrus.Debugf("Op - %s - completed successfully", opName)
			iClient.eventChan <- &meshes.EventsResponse{
				OperationId: arReq.OperationId,
				EventType:   meshes.EventType_INFO,
				Summary:     fmt.Sprintf("Linkerd %s successfully", opName),
				Details:     fmt.Sprintf("The latest version of Linkerd is now %s.", opName),
			}
		}()
		return &meshes.ApplyRuleResponse{
			OperationId: arReq.OperationId,
		}, nil
	case installBooksAppCommand:
		appName = "Linkerd Books App"
		svcName = "webapp"
		yamlFileContents, err = iClient.getYAML(booksAppInstallFile, booksAppLocalFile)
		if err != nil {
			return nil, err
		}
		fallthrough
	case installHTTPBinApp:
		if appName == "" {
			appName = "HTTP Bin App"
			svcName = "httpbin"
			yamlFileContents, err = iClient.executeTemplate(ctx, arReq.Username, arReq.Namespace, op.templateName)
			if err != nil {
				return nil, err
			}
		}
		fallthrough
	case installIstioBookInfoApp:
		if appName == "" {
			appName = "Istio canonical Book Info App"
			svcName = "productpage"
			yamlFileContents, err = iClient.executeTemplate(ctx, arReq.Username, arReq.Namespace, op.templateName)
			if err != nil {
				return nil, err
			}
		}
		fallthrough
	case installEmojiVotoCommand:
		if appName == "" {
			appName = "Emojivoto App"
			svcName = "web-svc"
			yamlFileContents, err = iClient.getYAML(emojivotoInstallFile, emojivotoLocalFile)
			if err != nil {
				return nil, err
			}
		}
		go func() {
			opName1 := "deploying"
			if arReq.DeleteOp {
				opName1 = "removing"
			}
			if !arReq.DeleteOp {
				if err := iClient.labelNamespaceForAutoInjection(ctx, arReq.Namespace); err != nil {
					iClient.eventChan <- &meshes.EventsResponse{
						OperationId: arReq.OperationId,
						EventType:   meshes.EventType_ERROR,
						Summary:     fmt.Sprintf("Error while %s the canonical %s", opName1, appName),
						Details:     err.Error(),
					}
					return
				}
			}
			if err := iClient.applyConfigChange(ctx, yamlFileContents, arReq.Namespace, arReq.DeleteOp); err != nil {
				iClient.eventChan <- &meshes.EventsResponse{
					OperationId: arReq.OperationId,
					EventType:   meshes.EventType_ERROR,
					Summary:     fmt.Sprintf("Error while %s the canonical %s", opName1, appName),
					Details:     err.Error(),
				}
				return
			}
			opName := "deployed"
			ports := []int64{}
			if arReq.DeleteOp {
				opName = "removed"
			} else {
				var err error
				ports, err = iClient.getSVCPort(ctx, svcName, arReq.Namespace)
				if err != nil {
					iClient.eventChan <- &meshes.EventsResponse{
						OperationId: arReq.OperationId,
						EventType:   meshes.EventType_WARN,
						Summary:     fmt.Sprintf("%s is deployed but unable to retrieve the port info for the service at the moment", appName),
						Details:     err.Error(),
					}
					return
				}
			}
			var portMsg string
			if len(ports) == 1 {
				portMsg = fmt.Sprintf("The service is possibly available on port: %v", ports)
			} else if len(ports) > 1 {
				portMsg = fmt.Sprintf("The service is possibly available on one of the following ports: %v", ports)
			}
			msg := fmt.Sprintf("%s is now %s. %s", appName, opName, portMsg)
			iClient.eventChan <- &meshes.EventsResponse{
				OperationId: arReq.OperationId,
				EventType:   meshes.EventType_INFO,
				Summary:     fmt.Sprintf("%s %s successfully", appName, opName),
				Details:     msg,
			}
		}()
		return &meshes.ApplyRuleResponse{
			OperationId: arReq.OperationId,
		}, nil
	default:
		err := fmt.Errorf("please select a valid operation")
		logrus.Error(err)
		return nil, err
	}

	if err := iClient.applyConfigChange(ctx, yamlFileContents, arReq.Namespace, arReq.DeleteOp); err != nil {
		return nil, err
	}

	return &meshes.ApplyRuleResponse{
		OperationId: arReq.OperationId,
	}, nil
}

func (iClient *Client) applyConfigChange(ctx context.Context, deploymentYAML, namespace string, deleteOpts bool) error {
	acceptedK8sTypes := regexp.MustCompile(`(Namespace|Role|ClusterRole|RoleBinding|ClusterRoleBinding|ServiceAccount|MutatingWebhookConfiguration|Secret|ValidatingWebhookConfiguration|APIService|PodSecurityPolicy|ConfigMap|Service|Deployment|CronJob|CustomResourceDefinition)`)
	sepYamlfiles := strings.Split(deploymentYAML, "\n---\n")
	mappingNamespace := &meta.RESTMapping{}
	dataNamespace := &unstructured.Unstructured{}
	for _, f := range sepYamlfiles {
		if f == "\n" || f == "" {
			// ignore empty cases
			continue
		}

		// Need to manually add the resources to the scheme &_&
		sch := runtime.NewScheme()
		_ = scheme.AddToScheme(sch)
		_ = apiextv1beta1.AddToScheme(sch)
		_ = apiregistrationv1.AddToScheme(sch)
		decode := serializer.NewCodecFactory(sch).UniversalDeserializer().Decode

		//decode := clientgoscheme.Codecs.UniversalDeserializer().Decode
		obj, groupVersionKind, err := decode([]byte(f), nil, nil)

		if err != nil {
			logrus.Debug(fmt.Sprintf("Error while decoding YAML object. Err was: %s", err))
			continue
		}

		if !acceptedK8sTypes.MatchString(groupVersionKind.Kind) {
			logrus.Debug(fmt.Sprintf("The custom-roles configMap contained K8s object types which are not supported! Skipping object with type: %s", groupVersionKind.Kind))
		} else {
			// convert the runtime.Object to unstructured.Unstructured
			gk := schema.GroupKind{
				Group: groupVersionKind.Group,
				Kind:  groupVersionKind.Kind,
			}
			groupResources, err := restmapper.GetAPIGroupResources(iClient.k8sClientset.Discovery())
			if err != nil {
				return nil
			}
			resm := restmapper.NewDiscoveryRESTMapper(groupResources)
			mapping, err := resm.RESTMapping(gk, groupVersionKind.Version)
			if err != nil {
				return nil
			}
			logrus.Debug(mapping)

			unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)

			if err != nil {
				return err
			}
			data := &unstructured.Unstructured{}
			data.SetUnstructuredContent(unstructuredObj)
			logrus.Debug(unstructuredObj)

			if mapping.Scope.Name() == "root" {
				if deleteOpts {
					if data.GetObjectKind().GroupVersionKind().Kind == "Namespace" {
						mappingNamespace = mapping
						dataNamespace = data
						continue
					}
					deletePolicy := metav1.DeletePropagationForeground
					t := int64(1)
					deleteOptions := &metav1.DeleteOptions{
						PropagationPolicy:  &deletePolicy,
						GracePeriodSeconds: &t,
					}
					err = iClient.k8sDynamicClient.Resource(mapping.Resource).Delete(data.GetName(), deleteOptions)
					if err != nil && !kubeerror.IsNotFound(err) {
						logrus.Info(fmt.Sprintf("Delete the %s %s failed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName()))
						return err
					}
					logrus.Info(fmt.Sprintf("Delete the %s %s succeed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName()))
				} else {
					_, err = iClient.k8sDynamicClient.Resource(mapping.Resource).Create(data, metav1.CreateOptions{})
					if err != nil && !kubeerror.IsAlreadyExists(err) {
						logrus.Info(fmt.Sprintf("Create the %s %s failed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName()))
						return err
					}
					logrus.Info(fmt.Sprintf("Create the %s %s succeed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName()))
				}
			} else {
				if deleteOpts {
					deletePolicy := metav1.DeletePropagationForeground
					deleteOptions := &metav1.DeleteOptions{
						PropagationPolicy: &deletePolicy,
					}
					err = iClient.k8sDynamicClient.Resource(mapping.Resource).Namespace(data.GetNamespace()).Delete(data.GetName(), deleteOptions)
					if err != nil && !kubeerror.IsNotFound(err) {
						logrus.Info(fmt.Sprintf("Delete the %s %s in namespace %s failed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName(), data.GetNamespace()))
						return err
					}

					logrus.Info(fmt.Sprintf("Delete the %s %s in namespace %s succeed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName(), data.GetNamespace()))

				} else {
					_, err = iClient.k8sDynamicClient.Resource(mapping.Resource).Namespace(data.GetNamespace()).Create(data, metav1.CreateOptions{})
					if err != nil && !kubeerror.IsAlreadyExists(err) {
						logrus.Info(fmt.Sprintf("Create the %s %s in namespace %s failed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName(), data.GetNamespace()))
						return err
					}
					logrus.Info(fmt.Sprintf("Create the %s %s in namespace %s succeed", data.GetObjectKind().GroupVersionKind().Kind, data.GetName(), data.GetNamespace()))
				}
			}

		}
	}
	// Remove the namespace at least.
	if deleteOpts && dataNamespace.GetName() != "default" {
		deletePolicy := metav1.DeletePropagationForeground
		deleteOptions := &metav1.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}
		err := iClient.k8sDynamicClient.Resource(mappingNamespace.Resource).Delete(dataNamespace.GetName(), deleteOptions)
		if err != nil {
			logrus.Info(fmt.Sprintf("Delete the %s %s failed", dataNamespace.GetObjectKind().GroupVersionKind().Kind, dataNamespace.GetName()))
			return err
		}
		logrus.Info(fmt.Sprintf("Delete the %s %s succeed", dataNamespace.GetObjectKind().GroupVersionKind().Kind, dataNamespace.GetName()))
	}
	return nil
}

// SupportedOperations - returns a list of supported operations on the mesh
func (iClient *Client) SupportedOperations(context.Context, *meshes.SupportedOperationsRequest) (*meshes.SupportedOperationsResponse, error) {
	supportedOpsCount := len(supportedOps)
	result := make([]*meshes.SupportedOperation, supportedOpsCount)
	i := 0
	for k, sp := range supportedOps {
		result[i] = &meshes.SupportedOperation{
			Key:      k,
			Value:    sp.name,
			Category: sp.opType,
		}
		i++
	}
	return &meshes.SupportedOperationsResponse{
		Ops: result,
	}, nil
}

// StreamEvents - streams generated/collected events to the client
func (iClient *Client) StreamEvents(in *meshes.EventsRequest, stream meshes.MeshService_StreamEventsServer) error {
	logrus.Debugf("waiting on event stream. . .")
	for {
		select {
		case event := <-iClient.eventChan:
			logrus.Debugf("sending event: %+#v", event)
			if err := stream.Send(event); err != nil {
				err = errors.Wrapf(err, "unable to send event")

				// to prevent loosing the event, will re-add to the channel
				go func() {
					iClient.eventChan <- event
				}()
				logrus.Error(err)
				return err
			}
		default:
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (iClient *Client) getSVCPort(ctx context.Context, svc, namespace string) ([]int64, error) {
	// web-svc
	ns := &unstructured.Unstructured{}
	res := schema.GroupVersionResource{
		Version:  "v1",
		Resource: "services",
	}
	ns.SetName(svc)
	ns.SetNamespace(namespace)
	ns, err := iClient.getResource(ctx, res, ns)
	if err != nil {
		err = errors.Wrapf(err, "unable to get service details")
		logrus.Error(err)
		return nil, err
	}
	svcInst := ns.UnstructuredContent()
	spec := svcInst["spec"].(map[string]interface{})
	ports, _ := spec["ports"].([]interface{})
	nodePorts := []int64{}
	for _, port := range ports {
		p, _ := port.(map[string]interface{})
		np, ok := p["nodePort"]
		if ok {
			npi, _ := np.(int64)
			nodePorts = append(nodePorts, npi)
		}
	}
	logrus.Debugf("retrieved svc: %+#v", ns)
	return nodePorts, nil
}
