/*
Copyright (C) 2019 Synopsys, Inc.

Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownership. The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License. You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied. See the License for the
specific language governing permissions and limitations
under the License.
*/

package synopsysctl

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blackducksoftware/synopsysctl/pkg/api"
	"github.com/blackducksoftware/synopsysctl/pkg/globals"
	"github.com/blackducksoftware/synopsysctl/pkg/util"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var restconfig *rest.Config
var kubeClient *kubernetes.Clientset

// setSynopsysctlLogLevel sets the binary's log level to the value stored in logLevelCtl
func setSynopsysctlLogLevel() error {
	lvl, err := log.ParseLevel(logLevelCtl)
	if err != nil {
		log.Errorf("ctl-log-Level '%s' is not a valid level: %s", logLevelCtl, err)
		return err
	}
	log.SetLevel(lvl)
	return nil
}

// setGlobalKubeConfigPath sets the global variable 'kubeConfigPath' with points to a kubeconfig file for accessing a cluster
// If the kubeconfig flag was set then kubeConfigPath should already have the path.
// If the kubeconfig flag was not set then it will check the environ 'KUBECONFIG' for a path
func setGlobalKubeConfigPath(cmd *cobra.Command) error {
	if !cmd.Flags().Lookup("kubeconfig").Changed { // if --kubeconfig flag wasn't set, check the environ
		if kubeconfigEnvVal, exists := os.LookupEnv("KUBECONFIG"); exists { // set kubeconfig if environ is set
			kubeConfigPath = kubeconfigEnvVal
		}
	}
	// if the kubeConfigPath was set, verify that the file exists
	if kubeConfigPath != "" {
		if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
			return fmt.Errorf("the kubeconfig path '%s' does not point to a file", kubeConfigPath)
		}
	}
	return nil
}

// GetKubeClientFromOutsideCluster returns the rest config of outside cluster
func GetKubeClientFromOutsideCluster(kubeconfigpath string, insecureSkipTLSVerify bool) (*rest.Config, error) {
	// Determine Config Paths
	if home := homeDir(); len(kubeconfigpath) == 0 && home != "" {
		kubeconfigpath = filepath.Join(home, ".kube", "config")
	}

	kubeConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: kubeconfigpath,
		},
		&clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server:                "",
				InsecureSkipTLSVerify: insecureSkipTLSVerify,
			},
		}).ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubeConfig, nil
}

// homeDir determines the user's home directory path
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

// setGlobalRestConfig sets the global variable 'restconfig' for other commands to use
func setGlobalRestConfig() error {
	var err error
	restconfig, err = GetKubeClientFromOutsideCluster(kubeConfigPath, insecureSkipTLSVerify)
	log.Debugf("rest config: %+v", restconfig)
	if err != nil {
		return err
	}
	return nil
}

// setGlobalKubeClient sets the global variable 'kubeClient' for other commands to use
func setGlobalKubeClient() error {
	var err error
	kubeClient, err = getKubeClient(restconfig)
	if err != nil {
		return err
	}
	return nil
}

// getKubeClient gets the kubernetes client
func getKubeClient(kubeConfig *rest.Config) (*kubernetes.Clientset, error) {
	client, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// DetermineClusterClients returns bool values for which client
// to use. They will never both be true
func DetermineClusterClients(restConfig *rest.Config, kubeClient *kubernetes.Clientset) (kube, openshift bool) {
	openshift = false
	kube = false

	kubectlPath := false
	ocPath := false
	_, exists := exec.LookPath("kubectl")
	if exists == nil {
		kubectlPath = true
	}
	_, ocexists := exec.LookPath("oc")
	if ocexists == nil {
		ocPath = true
	}

	// Add Openshift rules
	openshiftTest := false
	if util.IsOpenshift(kubeClient) {
		openshiftTest = true
	}

	if ocPath && openshiftTest { // if oc exists and the cluster is openshift
		log.Debugf("oc exists and the cluster is openshift")
		return false, true
	}
	if kubectlPath && !openshiftTest { // if kubectl exists and it isn't openshift
		log.Debugf("kubectl exists and it isn't openshift")
		return true, false
	}
	if kubectlPath && !ocPath && openshiftTest { // if kubectl exists, oc doesn't exist, and it is openshift
		log.Debugf("kubectl exists, oc doesn't exist, and it is openshift")
		return true, false
	}
	if ocPath && !kubectlPath && !openshiftTest { // If oc exists, kubectl doesn't exist, and it isn't openshift
		log.Debugf("oc exists, kubectl doesn't exist, and it isn't openshift")
		return false, true
	}
	return false, false // neither client exists
}

func getKubeExecCmd(restconfig *rest.Config, kubeClient *kubernetes.Clientset, args ...string) (*exec.Cmd, error) {
	kube, openshift := DetermineClusterClients(restconfig, kubeClient)

	// cluster-info in kube doesnt seem to be in
	// some versions of oc, but status is.
	// double check this.
	if args[0] == "cluster-info" && openshift {
		args[0] = "status"
	}
	// add global flags: insecure-skip-tls-verify and --kubeconfig
	if insecureSkipTLSVerify == true {
		args = append([]string{fmt.Sprintf("--insecure-skip-tls-verify=%t", insecureSkipTLSVerify)}, args...)
	}
	if kubeConfigPath != "" {
		args = append([]string{fmt.Sprintf("--kubeconfig=%s", kubeConfigPath)}, args...)
	}
	if openshift {
		return exec.Command("oc", args...), nil
	} else if kube {
		return exec.Command("kubectl", args...), nil
	} else {
		return nil, fmt.Errorf("couldn't determine if running in Openshift or Kubernetes")
	}
}

// RunKubeCmd is a simple wrapper to oc/kubectl exec that captures output.
// TODO consider replacing w/ go api but not crucial for now.
func RunKubeCmd(restconfig *rest.Config, kubeClient *kubernetes.Clientset, args ...string) (string, error) {
	cmd2, err := getKubeExecCmd(restconfig, kubeClient, args...)
	if err != nil {
		return "", err
	}

	stdoutErr, err := cmd2.CombinedOutput()
	if err != nil {
		return string(stdoutErr), err
	}
	return string(stdoutErr), nil
}

// RunKubeCmdWithStdin is a simple wrapper to kubectl exec command with standard input
func RunKubeCmdWithStdin(restconfig *rest.Config, kubeClient *kubernetes.Clientset, stdin string, args ...string) (string, error) {
	cmd2, err := getKubeExecCmd(restconfig, kubeClient, args...)
	if err != nil {
		return "", err
	}

	stdinPipe, err := cmd2.StdinPipe()
	if err != nil {
		return "", err
	}

	go func() {
		defer stdinPipe.Close()
		io.WriteString(stdinPipe, stdin)
	}()

	stdoutErr, err := cmd2.CombinedOutput()
	if err != nil {
		return string(stdoutErr), err
	}
	return string(stdoutErr), nil
}

// RunKubeEditorCmd is a wrapper for oc/kubectl but redirects
// input/output to the user - ex: let user control text editor
func RunKubeEditorCmd(restConfig *rest.Config, kubeClient *kubernetes.Clientset, args ...string) error {
	var cmd *exec.Cmd
	kube, openshift := DetermineClusterClients(restconfig, kubeClient)

	// cluster-info in kube doesnt seem to be in
	// some versions of oc, but status is.
	// double check this.
	if args[0] == "cluster-info" && openshift {
		args[0] = "status"
	}
	// add global flags: insecure-skip-tls-verify and --kubeconfig
	if insecureSkipTLSVerify == true {
		args = append([]string{fmt.Sprintf("--insecure-skip-tls-verify=%t", insecureSkipTLSVerify)}, args...)
	}
	if kubeConfigPath != "" {
		args = append([]string{fmt.Sprintf("--kubeconfig=%s", kubeConfigPath)}, args...)
	}
	if openshift {
		cmd = exec.Command("oc", args...)
	} else if kube {
		cmd = exec.Command("kubectl", args...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		return err
	}
	//time.Sleep(1 * time.Second) TODO why did Jay put this here???
	return nil
}

// KubectlApplyRuntimeObjects creates runtime objects by converting them to bytes
// and passing them through the kubectl command
func KubectlApplyRuntimeObjects(objects map[string]runtime.Object) error {
	var content []byte
	for _, obj := range objects {
		secretBytes, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		content = append(content, secretBytes...)
	}
	out, err := RunKubeCmdWithStdin(restconfig, kubeClient, string(content), "apply", "--validate=false", "-f", "-")
	if err != nil {
		return fmt.Errorf("failed to deploy Runtime Object: %+v : %+v", out, err)
	}
	return nil
}

// KubectlDeleteRuntimeObjects deletes runtime objects by converting them to bytes
// and passing them through the kubectl command
func KubectlDeleteRuntimeObjects(objects map[string]runtime.Object) error {
	var content []byte
	for _, obj := range objects {
		secretBytes, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		content = append(content, secretBytes...)
	}
	out, err := RunKubeCmdWithStdin(restconfig, kubeClient, string(content), "delete", "-f", "-")
	if err != nil {
		return fmt.Errorf("failed to delete Runtime Object: %+v : %+v", out, err)
	}
	return nil
}

// UpdateHelmChartLocation uses --app-resources-path and chartVersion to update the value at *chartVariable. This value is originally set
// when synopsysctl starts (see file pkg/globals/helmglobalvalues.go)
func UpdateHelmChartLocation(flags *pflag.FlagSet, chartName, appVersion string, chartVariable *string) error {
	chartLocationFlag := flags.Lookup("app-resources-path")
	if chartLocationFlag.Changed {
		*chartVariable = chartLocationFlag.Value.String()
	} else {
		if len(appVersion) > 0 {
			chartURL, err := util.GetLatestChartURLForAppVersion(globals.IndexChartURLs, chartName, appVersion)
			if err != nil {
				return fmt.Errorf("failed to get resources version for '%s': %+v", chartName, err)
			}
			*chartVariable = chartURL
		}
	}
	return nil
}

func cleanAlertHelmError(errString, releaseName, alertName string) string {
	helmName := fmt.Sprintf("release '%s'", releaseName)
	instanceName := fmt.Sprintf("instance '%s'", alertName)
	cleanErrorMsg := strings.Replace(errString, helmName, instanceName, 1)
	return cleanErrorMsg
}

// NewPVCVolume creates a new Persistent Volume Claim volume object
func NewPVCVolume(config api.PVCVolumeConfig) *api.Volume {
	v := corev1.Volume{
		Name: config.VolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: config.PVCName,
				ReadOnly:  config.ReadOnly,
			},
		},
	}

	return &api.Volume{&v}
}
